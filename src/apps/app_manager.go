package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/diskguard"
	"reagent/errdefs"
	"reagent/messenger/topics"
	"reagent/safe"
	"reagent/store"
	"reagent/tunnel"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// teardownLockAcquireTimeout bounds how long a REMOVED/UNINSTALLED request
// waits for an interrupted in-flight transition to unwind and release the
// transition lock. Generous because the un-cancelable stretches of a
// transition (a compose up/down, docker-API calls) can legitimately run for
// minutes; on expiry the teardown is dropped with an error and the operator's
// next attempt retries. A var, not a const, so tests can shorten it.
var teardownLockAcquireTimeout = time.Minute * 5

type AppManager struct {
	AppStore      *store.AppStore
	StateMachine  *StateMachine
	StateObserver *StateObserver
	tunnelManager tunnel.TunnelManager
	hostPorts     *HostPortRegistry
	crashLoops    map[*CrashLoop]struct{}
	crashLoopLock sync.Mutex

	// tunnelRebuilds paces the retries of every tunnel that failed to come up:
	// a proxy frps keeps refusing (see mayRebuildTunnel) or an add that failed
	// (see noteTunnelAdd). portSyncRetries holds the one follow-up re-sync per
	// app that drives those retries (see schedulePortSyncRetry). Both are
	// guarded by tunnelRebuildLock.
	tunnelRebuilds    map[string]*tunnelRebuildState
	portSyncRetries   map[appStageKey]*portSyncRetry
	tunnelRebuildLock sync.Mutex

	// reservationNotes paces the user-visible "takes effect on next restart"
	// app-log line for a host-port reservation applied to a running container:
	// once per (rule, reserved port), not once per sync pass. See
	// notePendingReservation.
	reservationNotes    map[hostPortKey]uint64
	reservationNoteLock sync.Mutex

	// databaseUnsynced is set when this start created the database file.
	// Until the first sync with the backend fills it, an empty database says
	// nothing about which containers are orphans: an agent that opened the
	// wrong database path would otherwise remove every app container.
	databaseUnsynced atomic.Bool
}

// MarkDatabaseUnsynced suspends the orphaned-container cleanup until
// MarkDatabaseSynced (see databaseUnsynced).
func (am *AppManager) MarkDatabaseUnsynced() { am.databaseUnsynced.Store(true) }

// MarkDatabaseSynced records that the database now holds the backend's
// requested app states, so orphans can be told apart again.
func (am *AppManager) MarkDatabaseSynced() { am.databaseUnsynced.Store(false) }

// Retry pacing for a tunnel that failed to come up: a proxy frps will not
// accept, or an add that failed outright (REtunnel refusing the remote port,
// frpc not reloading).
//
// A refusal is invisible to AddTunnel (it returns success on a successful frpc
// config reload), so the next sync finds the proxy dead and rebuilds it, which
// produces another refusal — an unbounded loop. When the refusal is
// appliance-side ("authorization check failed" while the tunnel container's
// WAMP session is down) the loop hammers hardest exactly when the appliance can
// least serve it, and rewrites frpc.yaml several times a second.
//
// Retrying is still right — the condition usually clears on its own — so this
// paces rather than stops: exponential backoff from rebuildBackoffBase to
// rebuildBackoffMax, reset the moment the proxy comes up. The agent also drives
// those retries itself (schedulePortSyncRetry) instead of waiting for the next
// sync: syncPortState otherwise runs only on a state push, a (re)connect, a
// state correction, a crashloop retry or the end of a transition, and none of
// those is tied to the failure clearing.
const (
	rebuildBackoffBase = 10 * time.Second
	rebuildBackoffMax  = 5 * time.Minute
)

type tunnelRebuildState struct {
	attempts int
	last     time.Time
	// loggedReservationFailure is the reserved-remote-port failure last
	// written to the app log for this tunnel in this agent run. A retry
	// re-reads the stored requested state, which never carries the
	// reservation_error persisted upstream back, so without it every retry
	// would write the same line again. Reset with the rest of the state once
	// the tunnel serves (noteTunnelHealthy).
	loggedReservationFailure string
}

// rebuildBackoff is how long a tunnel waits after its attempts-th failed
// attempt before the next one: rebuildBackoffBase, doubling per attempt, capped
// at rebuildBackoffMax.
func rebuildBackoff(attempts int) time.Duration {
	backoff := rebuildBackoffBase << (attempts - 1)
	if backoff > rebuildBackoffMax || backoff <= 0 { // <= 0 guards the shift overflowing
		backoff = rebuildBackoffMax
	}
	return backoff
}

// appStageKey identifies one installed app: an app key is installed at most
// once per stage.
type appStageKey struct {
	AppKey uint64
	Stage  common.Stage
}

// portSyncRetry is the follow-up re-sync pending for one app.
type portSyncRetry struct {
	timer *time.Timer
	due   time.Time
}

// portSyncRetryAfterFunc arms the timer of a port-sync retry. A var, not a
// direct time.AfterFunc call, so tests can capture retries and fire them by
// hand.
var portSyncRetryAfterFunc = time.AfterFunc

// diskEmergency reports whether the device is in a disk emergency (see package
// diskguard). A var so tests can put the device into one; only diskguard can.
var diskEmergency = diskguard.IsEmergency

func NewAppManager(sm *StateMachine, as *store.AppStore, so *StateObserver, tm tunnel.TunnelManager) *AppManager {
	am := AppManager{
		StateMachine:    sm,
		StateObserver:   so,
		AppStore:        as,
		tunnelManager:   tm,
		hostPorts:       NewHostPortRegistry(),
		crashLoops:      make(map[*CrashLoop]struct{}),
		tunnelRebuilds:  make(map[string]*tunnelRebuildState),
		portSyncRetries: make(map[appStageKey]*portSyncRetry),
	}

	am.StateObserver.AppManager = &am
	am.hostPorts.SetReservationsProvider(am.reservedHostPorts)
	return &am
}

// reservedHostPorts lists every user-pinned host port across ALL installed
// apps, read live from the requested-state rows the cloud syncs down (no
// one-shot boot seeding: reservations made while an app is stopped — which
// has no registry assignment — must still block the pool and availability
// checks). Invoked by the registry under its own lock; must not call back
// into it.
func (am *AppManager) reservedHostPorts() []reservedHostPort {
	payloads, err := am.AppStore.GetRequestedStates()
	if err != nil {
		log.Debug().Err(err).Msg("Could not read requested states for host port reservations")
		return nil
	}

	reservations := make([]reservedHostPort, 0)
	for _, statePayload := range payloads {
		rules, err := tunnel.InterfaceToPortForwardRule(statePayload.Ports)
		if err != nil {
			continue
		}
		for _, rule := range rules {
			if reservedHostPortFor(statePayload.Stage, rule) == 0 {
				continue
			}
			reservations = append(reservations, reservedHostPort{
				Protocol: wireProtocol(rule.Protocol),
				Port:     rule.ReservedHostPort,
				Owner:    hostPortKeyForRule(statePayload.Stage, statePayload.AppKey, rule),
			})
		}
	}
	return reservations
}

func (am *AppManager) syncPortState(payload common.TransitionPayload, app *common.App) error {
	log.Debug().Str("app", payload.AppName).Msg("syncPortState called")
	globalConfig := am.StateMachine.Container.GetConfig()

	if payload.Stage == common.DEV {
		log.Debug().Msg("Skipping syncPortState for DEV stage")
		return nil
	}

	// Degrade cleanly when tunnels are unavailable (frpc missing/quarantined,
	// or the device can't reach the tunnel server): skip the frpc calls, never
	// error, so app starts are not blocked by an absent tunnel feature.
	//
	// Only the tunnel half is skipped. Host ports are assigned at container
	// launch whether or not tunnels work, and they are how a user on the same
	// network reaches the app — so they must still be resolved and persisted
	// upstream, otherwise the UI has no port to show on exactly the devices
	// where the LAN address is the only way in.
	tunnelsAvailable := am.tunnelManager.TunnelCapable()
	if !tunnelsAvailable {
		log.Warn().Msg("Tunneling feature is unavailable on this device; syncing host ports only")
	}

	app.StateLock.Lock()
	curAppState := app.CurrentState
	requestedState := app.RequestedState
	app.StateLock.Unlock()

	portRules, err := tunnel.InterfaceToPortForwardRule(payload.Ports)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to convert interface to port forward rule")
		return err
	}

	if len(portRules) == 0 {
		// Nothing to reconcile or persist: the rule list this would write back
		// is the one the cloud just sent us.
		return nil
	}

	newPorts := make([]common.PortForwardRule, 0)

	// The earliest any tunnel of this app needs another look: one that failed
	// to come up in this pass is retried by the agent itself (see "Retry
	// pacing"), at most one re-sync per app.
	var resyncIn time.Duration
	resyncNeeded := false
	needResync := func(after time.Duration) {
		if !resyncNeeded || after < resyncIn {
			resyncIn, resyncNeeded = after, true
		}
	}

	for _, portRule := range portRules {
		subdomain := tunnel.CreateSubdomain(tunnel.Protocol(portRule.Protocol), uint64(globalConfig.ReswarmConfig.DeviceKey), payload.AppName, portRule.Port)
		tunnelID := tunnel.CreateTunnelID(subdomain, portRule.Protocol)
		newConfig := tunnel.TunnelConfig{}

		// A user-pinned remote port is the desired state; it only applies to
		// PROD tcp/udp rules (http/https and DEV carry reserved_* inert).
		reservedRemote := uint64(0)
		if reservationsApply(payload.Stage, portRule) {
			reservedRemote = portRule.ReservedRemotePort
		}
		wantRemote := portRule.RemotePort
		if reservedRemote != 0 {
			wantRemote = reservedRemote
		}

		// The port frpc dials: the agent-managed host port the declared port
		// is published on. Rules with an explicit LocalIP point away from the
		// app container and keep their declared port.
		dialPort, dialPortKnown := portRule.Port, true
		reservationFailure := ""
		if portRule.LocalIP == "" {
			dialPort, dialPortKnown, reservationFailure = am.resolveTunnelHostPort(payload, portRule)
		}

		if portRule.Active {
			if tunnelsAvailable && (requestedState == common.RUNNING || curAppState == common.RUNNING) {
				if !dialPortKnown {
					// Fresh app that has not been started under managed ports
					// yet. The post-transition syncPortState creates the
					// tunnel once the launch path has allocated the port.
					log.Debug().Str("tunnelID", tunnelID).Msg("No host port allocated yet, deferring tunnel creation")
				} else {
					tunnelConfig := tunnel.TunnelConfig{
						Subdomain:  subdomain,
						AppName:    payload.AppName,
						Protocol:   tunnel.Protocol(portRule.Protocol),
						LocalPort:  dialPort,
						LocalIP:    portRule.LocalIP,
						RemotePort: wantRemote,
						Reserved:   reservedRemote != 0,
					}

					tnl := am.tunnelManager.Get(tunnelID)

					// "Already exists" must mean an actual live frpc proxy,
					// not a bookkeeping entry. The in-memory map survives
					// remove/reinstall cycles and frps-side drops, and
					// trusting it deadlocked exactly the tunnels the user
					// needed back: the device skipped the add on every
					// restart because it believed the tunnel was up, while
					// nothing was connected — and nothing else ever took the
					// initiative to rebuild it.
					//
					// A dead proxy under matching bookkeeping is the only case
					// worth pacing: a brand-new tunnel, and one whose local port
					// genuinely changed, are real work and rebuild immediately.
					// deferRebuild holds this pass off without touching the
					// stored config — see mayRebuildTunnel. A live tunnel whose
					// remote port contradicts a non-zero reservation is stale
					// the same way a wrong local port is: applying or changing
					// a reservation must move a running tunnel (and drop the
					// old frps bind promptly), so it takes the Remove+Add path.
					alive, deferRebuild, rebuilding := false, false, false
					if tnl != nil && tnl.Config.LocalPort == dialPort &&
						(reservedRemote == 0 || tnl.Config.RemotePort == reservedRemote) {
						var reason string
						alive, reason = am.tunnelProxyAlive(tunnelID)
						if alive {
							am.noteTunnelHealthy(tunnelID)
						} else {
							ok, retryIn := am.mayRebuildTunnel(tunnelID)
							deferRebuild, rebuilding = !ok, ok
							if deferRebuild {
								// Come back the moment the backoff allows the
								// rebuild; no sync would otherwise.
								needResync(retryIn)
							}
							log.Warn().Str("tunnelID", tunnelID).Str("reason", reason).
								Bool("rebuilding", ok).Dur("retryIn", retryIn).
								Msg("Tunnel bookkeeping says up but frpc has no live proxy")
						}
					}

					// Deferring takes the same result as the alive path: the
					// stored config carries the remote port frps reserved, and
					// persisting the zero config of a skipped add would both
					// hide the address upstream and make the next agent start
					// reserve a different port.
					if alive || deferRebuild {
						log.Debug().Str("tunnelID", tunnelID).Bool("deferred", deferRebuild).
							Msg("Skipping tunnel add")
						newConfig = tnl.Config
					} else {
						if tnl != nil {
							// Either the app was republished on a different
							// host port (frpc must not keep dialing the stale
							// one), or the proxy is dead and gets rebuilt.
							log.Info().Str("tunnelID", tunnelID).Uint64("oldPort", tnl.Config.LocalPort).Uint64("newPort", dialPort).Msg("Replacing tunnel")
							err := am.tunnelManager.RemoveTunnel(tnl.Config)
							if err != nil {
								log.Error().Stack().Err(err).Msg("Failed to remove outdated tunnel")
							}
						}

						added, addErr := am.tunnelManager.AddTunnel(tunnelConfig)
						if after, ok := am.noteTunnelAdd(tunnelID, rebuilding, addErr != nil); ok {
							needResync(after)
						}
						if addErr != nil {
							// Keep the incoming rule as-is rather than
							// persisting the zero config a failed add returns.
							log.Error().Stack().Err(addErr).Msg("Failed to add tunnel")
							if reservedRemote != 0 {
								// A reserved rule must not fail silently: the
								// UI row would spin forever on a log-only
								// failure. Surface it on the rule and in the
								// app log — but only when the failure actually
								// changed: syncPortState runs on every sync
								// pass, and a persisting frps refusal (already
								// persisted on the incoming rule) must not
								// write the same line into the user-visible
								// app log once per pass forever. The agent's
								// own retries re-read the stored requested
								// state, which never carries that persisted
								// error back, so the failure last logged for
								// this tunnel counts as already said too.
								reservationFailure = addErr.Error()
								if reservationFailure != portRule.ReservationError && am.noteReservationFailure(tunnelID, reservationFailure) {
									am.writeAppLog(payload.ContainerName.Prod, fmt.Sprintf("Reserved tunnel port %d for port %d could not be established: %v. Reserved ports are never reassigned automatically - free the port or change the reservation.", reservedRemote, portRule.Port, addErr))
								}
							}
						} else {
							newConfig = added
						}
					}
				}
				// continue
			}
		} else if tunnelsAvailable {
			// Remove tunnel when it's not active (regardless of app state).
			// Skip entirely when there is nothing to remove — the old code
			// re-ran RemoveTunnel (a config rewrite + frpc reload) for every
			// inactive rule on every sync, forever.
			if am.tunnelManager.Get(tunnelID) == nil && !am.tunnelInConfigFile(tunnelID) {
				newPorts = append(newPorts, portRule)
				continue
			}

			// Build the tunnel config to remove it from the file even if it's not in memory
			tunnelConfig := tunnel.TunnelConfig{
				Subdomain:  subdomain,
				AppName:    payload.AppName,
				Protocol:   tunnel.Protocol(portRule.Protocol),
				LocalPort:  portRule.Port,
				LocalIP:    portRule.LocalIP,
				RemotePort: portRule.RemotePort,
			}

			log.Info().Str("tunnelID", tunnelID).Msg("Removing tunnel as it is not active")
			err := am.tunnelManager.RemoveTunnel(tunnelConfig)
			if err != nil {
				log.Error().Stack().Err(err).Msg("Failed to remove tunnel")
			}
		}

		newPort := portRule
		if reservationFailure != "" {
			newPort.ReservationError = reservationFailure
		}
		if newConfig.RemotePort != 0 {
			if reservedRemote != 0 && newConfig.RemotePort != reservedRemote {
				// Never persist a granted remote port that contradicts the
				// user's reservation: the reserved value stays the desired
				// state and the mismatch is surfaced instead.
				newPort.RemotePort = reservedRemote
				newPort.ReservationError = fmt.Sprintf("tunnel granted port %d instead of reserved %d", newConfig.RemotePort, reservedRemote)
				log.Warn().Str("app", payload.AppName).Uint64("granted", newConfig.RemotePort).Uint64("reserved", reservedRemote).Msg("Tunnel grant contradicts the reserved remote port")
			} else {
				if newPort.RemotePort != newConfig.RemotePort {
					newPort.RemotePort = newConfig.RemotePort
					log.Info().Str("app", payload.AppName).Int("localPort", int(portRule.Port)).Int("remotePort", int(newPort.RemotePort)).Msg("Assigned new remote port for tunnel")
				}
				// A grant matching the reservation (or no reservation at all)
				// clears any stale failure from a previous pass.
				newPort.ReservationError = ""
			}
		}
		if dialPortKnown && portRule.LocalIP == "" && newPort.HostPort != dialPort {
			// Persist the host port to t_device_to_app.ports: shown in the
			// UI and returned on the next sync for recovery.
			newPort.HostPort = dialPort
			log.Info().Str("app", payload.AppName).Int("port", int(portRule.Port)).Int("hostPort", int(dialPort)).Msg("Assigned host port for app port")
		}
		newPorts = append(newPorts, newPort)

	}

	// A pass that looked at the tunnels and found none needing another look
	// makes a pending re-sync moot. While the device cannot tunnel nothing was
	// looked at, so a pending one is left alone; the capability coming back
	// reconciles every app anyway (SyncAllPortStates).
	if tunnelsAvailable {
		if resyncNeeded {
			am.schedulePortSyncRetry(payload.AppKey, payload.Stage, resyncIn)
		} else {
			am.cancelPortSyncRetry(payload.AppKey, payload.Stage)
		}
	}

	np, err := tunnel.PortForwardRuleToInterface(newPorts)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to convert newPorts to interface")
		return err
	}

	payload.Ports = np

	// Persist upstream only when reconciliation actually changed a rule. The
	// incoming rules ARE the cloud's current state, so an unchanged result has
	// nothing to say — and this runs on every transition request, including
	// every crashloop retry, where an unconditional write logged "Remote port
	// saved successfully" for an app that never started.
	if !reflect.DeepEqual(newPorts, portRules) {
		_ = am.tunnelManager.SaveRemotePorts(payload)
	}

	// Live-update the bind-mounted env files ({NAME}.txt / {NAME}_CLOUD.txt)
	// so running containers see fresh tunnel ports without a restart.
	refreshRemotePortEnvFiles(globalConfig, payload.Stage, payload.AppName, newPorts)

	// err = am.AppStore.UpdateLocalRequestedState(payload)
	// if err != nil {
	// 	return err
	// }

	if tunnelsAvailable {
		_ = am.UpdateTunnelState()
	}

	return nil
}

// tunnelProxyAlive reports whether frpc actually serves a proxy for this
// tunnel id right now, and — when it does not — WHY. The in-memory bookkeeping
// alone is not evidence: see syncPortState. "wait start" counts as alive (frpc
// is bringing it up; a rebuild would only flap it).
//
// The reason is frps' verbatim refusal, which frpc keeps in the proxy's Err
// field ("authorization check failed", "port already used", …). That string is
// the only record of the refusal anywhere on the device — AddTunnel reports
// success as soon as the config reload succeeds, so nothing else on this side
// ever learns that frps said no. Discarding it turned every refusal into an
// unexplained rebuild loop.
func (am *AppManager) tunnelProxyAlive(tunnelID string) (bool, string) {
	status, err := am.tunnelManager.Status(tunnelID)
	if err != nil {
		return false, fmt.Sprintf("frpc status unavailable: %s", err)
	}
	if status.Error != "" {
		return false, status.Error
	}
	switch status.Status {
	case "running", "wait start":
		return true, ""
	}
	return false, fmt.Sprintf("frpc proxy status %q", status.Status)
}

// mayRebuildTunnel reports whether a dead tunnel is due for another rebuild
// attempt, recording the attempt when it says yes. Applies ONLY to the
// rebuild-a-dead-proxy path: a brand-new tunnel and a tunnel whose local port
// actually changed are real work and are never held off (their failures are
// counted by noteTunnelAdd).
func (am *AppManager) mayRebuildTunnel(tunnelID string) (bool, time.Duration) {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	st := am.tunnelRebuilds[tunnelID]
	if st == nil {
		am.tunnelRebuilds[tunnelID] = &tunnelRebuildState{attempts: 1, last: time.Now()}
		return true, 0
	}

	backoff := rebuildBackoff(st.attempts)
	if waited := time.Since(st.last); waited < backoff {
		return false, backoff - waited
	}

	st.attempts++
	st.last = time.Now()
	return true, 0
}

// noteTunnelAdd counts an add toward the tunnel's retry pacing and returns how
// long until a follow-up re-sync should look at the tunnel again, or false
// when it needs none.
//
// paced means mayRebuildTunnel already counted this attempt (the rebuild of a
// dead proxy). Every other add — a brand-new tunnel, a moved local or dial
// port, a live tunnel moved to its reserved remote port — is never held off by
// the backoff (whatever asked for the sync gets the add at once) and is
// counted here: always when it failed, and when it succeeded only for a tunnel
// that failed before, because frps may still refuse the proxy and only a later
// liveness check can tell. A first-time add that succeeds counts nothing, so a
// normal boot schedules no extra syncs.
func (am *AppManager) noteTunnelAdd(tunnelID string, paced bool, failed bool) (time.Duration, bool) {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	st := am.tunnelRebuilds[tunnelID]
	if !paced {
		if st == nil {
			if !failed {
				return 0, false
			}
			st = &tunnelRebuildState{}
			am.tunnelRebuilds[tunnelID] = st
		}
		st.attempts++
		st.last = time.Now()
	}
	if st == nil {
		// A concurrent sync saw the proxy serving and cleared the pacing.
		return 0, false
	}
	return rebuildBackoff(st.attempts), true
}

// noteReservationFailure reports whether a reserved tunnel's failure still has
// to be written to the app log, and records it as written: false when it is
// the failure last logged for this tunnel in this agent run.
func (am *AppManager) noteReservationFailure(tunnelID string, failure string) bool {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	st := am.tunnelRebuilds[tunnelID]
	if st == nil {
		// A concurrent sync saw the proxy serving: nothing to compare against.
		return true
	}
	if st.loggedReservationFailure == failure {
		return false
	}
	st.loggedReservationFailure = failure
	return true
}

// noteTunnelHealthy clears the backoff for a tunnel that is serving again, so
// the next genuine failure starts from the base delay rather than a stale one.
func (am *AppManager) noteTunnelHealthy(tunnelID string) {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()
	delete(am.tunnelRebuilds, tunnelID)
}

// schedulePortSyncRetry has the agent re-sync an app's tunnels by itself
// within after: one pending re-sync per app, kept at the earliest time any of
// its tunnels asked for.
//
// Nothing else would retry a tunnel that failed to come up: syncPortState
// runs only when a state push arrives, the agent (re)connects, a state
// observer corrects the app, a crashloop retries or a transition ends, and
// none of those is tied to the failure clearing. On 2026-10-02, after an agent
// update, REtunnel refused re.tunnel.expose_port for the tunnels' previous
// remote ports ("not authorized": it had lost its port-owner record in a pod
// restart). The refusal cleared within minutes, but the TCP/UDP tunnels on
// devices 4749 and 4806 stayed down until their apps were restarted by hand.
func (am *AppManager) schedulePortSyncRetry(appKey uint64, stage common.Stage, after time.Duration) {
	key := appStageKey{AppKey: appKey, Stage: stage}
	due := time.Now().Add(after)

	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	if pending := am.portSyncRetries[key]; pending != nil {
		if !pending.due.After(due) {
			return
		}
		pending.timer.Stop()
	}

	retry := &portSyncRetry{due: due}
	retry.timer = portSyncRetryAfterFunc(after, func() {
		am.tunnelRebuildLock.Lock()
		if am.portSyncRetries[key] == retry {
			delete(am.portSyncRetries, key)
		}
		am.tunnelRebuildLock.Unlock()

		log.Info().Uint64("app_key", appKey).Str("stage", string(stage)).Msg("Re-syncing an app's tunnels: one of them failed to come up")
		// Outside tunnelRebuildLock: syncPortState takes it itself.
		safe.Run(func() { am.retryPortSync(appKey, stage) })
	})
	am.portSyncRetries[key] = retry
}

// cancelPortSyncRetry drops an app's pending re-sync once a pass found none of
// its tunnels needing another look.
func (am *AppManager) cancelPortSyncRetry(appKey uint64, stage common.Stage) {
	key := appStageKey{AppKey: appKey, Stage: stage}

	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	if pending := am.portSyncRetries[key]; pending != nil {
		pending.timer.Stop()
		delete(am.portSyncRetries, key)
	}
}

// retryPortSync re-runs syncPortState for one app from its locally stored
// requested state: the latest the agent knows, so a rule that turned inactive,
// or an app no longer wanted RUNNING, reconciles to that instead of being
// re-added.
//
// It stands down during a disk emergency. diskguard then stops every app
// container and InitTransition refuses to start one, so re-adding their tunnels
// would only reload frpc and rewrite frpc.yaml on a critically full disk, for
// apps that are not running: os.WriteFile truncates before it writes, so a
// write that hits ENOSPC leaves the file short, and the next reload drops
// tunnels that were up. A re-sync skipped here is not lost: leaving the
// emergency reconciles every app's tunnels (diskguard's OnRecover runs
// SyncAllPortStates).
func (am *AppManager) retryPortSync(appKey uint64, stage common.Stage) {
	if diskEmergency() {
		log.Info().Uint64("app_key", appKey).Str("stage", string(stage)).
			Msg("Not re-syncing an app's tunnels during the disk emergency; they are reconciled once it clears")
		return
	}

	app, err := am.AppStore.GetApp(appKey, stage)
	if err != nil || app == nil {
		// Uninstalled or removed meanwhile: nothing left to retry.
		return
	}

	payload, err := am.AppStore.GetRequestedState(appKey, stage)
	if err != nil {
		// A completed teardown deletes the row; the in-memory app outlives it.
		return
	}

	err = am.syncPortState(payload, app)
	if err != nil {
		log.Error().Stack().Err(err).Str("app", payload.AppName).Msg("Failed to re-sync port state")
	}
}

// SyncAllPortStates re-syncs the tunnels of every PROD app from its stored
// requested state, one app after another. The tunnel manager runs it whenever
// the device becomes able to tunnel (see tunnel.FrpTunnelManager.SetOnAvailable):
// tunnels skipped while it could not, and adds that failed while frpc was
// starting, are reconciled the moment it can tunnel again. Nothing else would
// ask for them. diskguard's OnRecover runs it when a disk emergency clears,
// for the re-syncs retryPortSync skipped during it.
func (am *AppManager) SyncAllPortStates() {
	if diskEmergency() {
		// Leaving the emergency runs this again (diskguard's OnRecover).
		log.Info().Msg("Not reconciling the apps' tunnels during the disk emergency; they are reconciled once it clears")
		return
	}

	payloads, err := am.AppStore.GetRequestedStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to read requested states to reconcile tunnels")
		return
	}

	log.Info().Msg("Reconciling the tunnels of every app")
	for _, payload := range payloads {
		// syncPortState never tunnels DEV apps.
		if payload.Stage != common.PROD {
			continue
		}
		am.retryPortSync(payload.AppKey, payload.Stage)
	}
}

// tunnelInConfigFile reports whether the frpc config file still carries a
// proxy for this tunnel id (covers entries left by a previous agent run that
// the in-memory map never saw).
func (am *AppManager) tunnelInConfigFile(tunnelID string) bool {
	configs, err := am.tunnelManager.GetTunnelConfig()
	if err != nil {
		// Can't prove absence — let the removal path run.
		return true
	}
	for _, cfg := range configs {
		if tunnel.CreateTunnelID(cfg.Subdomain, string(cfg.Protocol)) == tunnelID {
			return true
		}
	}
	return false
}

// RemoveAppTunnels tears down every tunnel belonging to an app — memory,
// config file and frpc alike. Called on uninstall: without it the app's
// proxies survived removal, kept dialing freed host ports, and their stale
// bookkeeping made the next install of the same app skip recreating them.
// File-driven (GetTunnelConfig) so proxies left over from a previous agent
// run are removed too.
func (am *AppManager) RemoveAppTunnels(appName string) {
	if !am.tunnelManager.TunnelCapable() {
		return
	}
	configs, err := am.tunnelManager.GetTunnelConfig()
	if err != nil {
		log.Error().Stack().Err(err).Str("app", appName).Msg("Failed to list tunnel configs for app removal")
		return
	}
	for _, cfg := range configs {
		if !strings.EqualFold(cfg.AppName, appName) {
			continue
		}
		if err := am.tunnelManager.RemoveTunnel(cfg); err != nil {
			log.Error().Stack().Err(err).Str("app", appName).Str("subdomain", cfg.Subdomain).Msg("Failed to remove app tunnel on uninstall")
		}
	}
}

func (am *AppManager) UpdateTunnelState() error {
	updateTopic := common.BuildTunnelStateUpdate(am.StateMachine.Container.GetConfig().ReswarmConfig.SerialNumber)
	tunnelStates, err := am.tunnelManager.GetState()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get tunnel states")
		return err
	}

	var args []interface{}
	for _, tunnelState := range tunnelStates {
		args = append(args, tunnelState)
	}

	log.Debug().Msg("Publishing tunnel state update")
	return am.AppStore.Messenger.Publish(topics.Topic(updateTopic), args, nil, nil)
}

func (am *AppManager) RequestAppState(payload common.TransitionPayload) error {
	app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get app in RequestAppState")
		return err
	}

	if app == nil {
		return fmt.Errorf("app (%d, %s) does not exist in the local app store", payload.AppKey, payload.Stage)
	}

	app.StateLock.Lock()
	curAppState := app.CurrentState
	requestedAppState := app.RequestedState
	app.StateLock.Unlock()

	// "to" is what THIS payload asks for; the store's requested state is logged
	// separately. Logging the store value as "to" once masked a whole outage
	// class: crashloop retries of a stale RUNNING payload logged as "to
	// UNINSTALLED" after an uninstall updated the store, making it look like
	// the uninstall was being attempted when it never ran.
	log.Debug().Str("app", payload.AppName).Str("from", string(curAppState)).Str("to", string(payload.RequestedState)).Str("storedRequestedState", string(requestedAppState)).Bool("retrying", payload.Retrying).Msg("Received Requested State")

	// clear crashloop counter if changing state request
	if !payload.Retrying {
		am.clearCrashLoop(app.AppKey, app.Stage)
	}

	// TODO: get rid of this ugly patch: cancel any filetransfers for this container on stop press
	if (curAppState == common.UNINSTALLED || curAppState == common.REMOVED || curAppState == common.PRESENT || curAppState == common.FAILED) &&
		(requestedAppState == common.PRESENT || requestedAppState == common.BUILT) && payload.Stage == common.DEV {
		log.Debug().Msg("Canceling file transfer due to state change")
		am.StateMachine.Filesystem.CancelFileTransfer(payload.ContainerName.Dev)
	}

	if payload.CancelTransition {
		log.Debug().Msgf("Cancel request was received for %s (%s) (currently: %s)", app.AppName, app.Stage, app.CurrentState)
		am.StateMachine.CancelTransition(app, payload)
		return nil
	}

	// An update is genuinely in progress once the app is UPDATING: the update
	// transition holds the lock for its whole duration. Refuse to pile new
	// transitions onto it. A "keep going" request (RUNNING, BUILT, ...) would be
	// dropped by the lock anyway and, worse, could re-drive the update in a loop
	// via VerifyState's update recovery — so drop it. PRESENT (settle) and the
	// teardown states (REMOVED, UNINSTALLED) are the escape hatches: they cancel
	// the in-flight update first, which unwinds it and releases the lock, then run
	// the requested terminal transition.
	if curAppState == common.UPDATING {
		switch payload.RequestedState {
		case common.PRESENT, common.REMOVED, common.UNINSTALLED:
			log.Info().Msgf("%s requested while updating %s (%s); canceling the in-flight update first", payload.RequestedState, app.AppName, app.Stage)
			am.StateMachine.CancelTransition(app, payload)
			return nil
		default:
			log.Info().Msgf("Update in progress for %s (%s); dropping request to %s", app.AppName, app.Stage, payload.RequestedState)
			return nil
		}
	}

	// Its own call, not folded into syncPortState: that one early-returns for
	// DEV, and DEV apps need their credential refreshed just as much.
	refreshAppCredentialEnvFiles(am.StateMachine.Container.GetConfig(), am.StateMachine.AppCredKey(), payload)

	err = am.syncPortState(payload, app)
	if err != nil {
		log.Error().Stack().Err(err).Msgf("failed to sync port state")
		return err
	}

	locked := app.SecureTransition() // if the app is not locked, it will lock the app
	if locked {
		isTeardownRequest := payload.RequestedState == common.REMOVED || payload.RequestedState == common.UNINSTALLED
		if !isTeardownRequest {
			log.Info().Msgf("App with name %s and stage %s is already transitioning, dropping request to %s. (CURRENT STATE: %s)", app.AppName, app.Stage, payload.RequestedState, curAppState)
			return nil
		}

		// A teardown (REMOVED/UNINSTALLED) must never be silently dropped: the
		// backend does not re-push it, so a drop can leave an app impossible to
		// uninstall (seen in the field with an app crashlooping on an
		// unreachable registry — every uninstall push landed inside a retry's
		// registry-login window and was dropped). Interrupt the in-flight
		// transition's cancelable work so it unwinds quickly, then take the
		// lock and run the teardown. Mirrors the UPDATING escape hatch above
		// for every other in-flight transition.
		log.Info().Msgf("%s requested while %s (%s) is transitioning; interrupting the transition and waiting for the lock", payload.RequestedState, app.AppName, app.Stage)
		am.StateMachine.interruptActiveTransition(payload)

		acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), teardownLockAcquireTimeout)
		err = app.TransitionLock.Acquire(acquireCtx, 1)
		cancelAcquire()
		if err != nil {
			log.Error().Err(err).Msgf("gave up waiting for the in-flight transition of %s (%s); teardown to %s was not executed", app.AppName, app.Stage, payload.RequestedState)
			return err
		}
	}

	// need to call this after we have secured the lock
	// to not change the actual state in the middle of an ongoing transition
	// this is necessary because some state transitions require a change of actual state (BUILD & PUBLISH)
	err = am.UpdateCurrentAppState(payload)
	if err != nil {
		app.UnlockTransition()
		log.Error().Stack().Err(err).Msg("Failed to update current app state")
		return err
	}

	// before we transition, should request the token
	// For PROD apps, use the device owner's account key to ensure proper permissions
	// This avoids issues when the original requestor's permissions have changed
	tokenAccountKey := payload.RequestorAccountKey
	if payload.Stage == common.PROD && payload.DeviceOwnerAccountKey != 0 {
		tokenAccountKey = payload.DeviceOwnerAccountKey
	}
	token, err := am.AppStore.GetRegistryToken(tokenAccountKey)
	if err != nil {
		app.UnlockTransition()
		log.Error().Stack().Err(err).Msg("Failed to get registry token")
		// The token comes from the backend, which can be unreachable or not
		// registered yet: after a router restart the agent reconnects and
		// re-drives its apps before RESWARM has re-registered. A crashloop
		// retry re-arms itself (driveRetry), but a fresh request has no loop
		// behind it, so an app that crashed in that window stayed FAILED until
		// the next push. Hand it to a crashloop like a failed transition.
		if !payload.Retrying && shouldEnterCrashLoop(payload.Stage, err) {
			am.incrementCrashLoop(payload)
		}
		return err
	}

	payload.RegisteryToken = token

	// Snapshot the state the transition actually starts from. The error path
	// below has already mutated app.CurrentState to FAILED by the time it logs,
	// so reading the live value there reported every failure as "from FAILED"
	// regardless of the transition that ran.
	app.StateLock.Lock()
	fromState := app.CurrentState
	app.StateLock.Unlock()

	errC := am.StateMachine.InitTransition(app, payload)
	if errC == nil {
		// not yet implemented or nullified state transition
		app.UnlockTransition()
		log.Debug().Msg("InitTransition returned nil channel, nothing to do")
		return nil
	}

	// block till transition has finished
	select {
	case err := <-errC:
		app.UnlockTransition()

		if errdefs.IsNoActionTransition(err) {
			log.Debug().Msg("A no action transition was executed, nothing to do. Will also not verify")

			// Ensure the remote state == current state
			err := am.StateObserver.Notify(app, app.CurrentState)
			if err != nil {
				log.Error().Stack().Err(err).Msgf("failed to ensure remote state")
				return err
			}

			_ = am.UpdateTunnelState()

			return nil
		}

		isCanceled := errdefs.IsDockerStreamCanceled(err)
		if err == nil || isCanceled {
			if !isCanceled {
				log.Info().Msgf("Successfully finished transaction for App (%s, %s)", app.AppName, app.Stage)
			} else {
				log.Info().Msgf("Successfully canceled transition for App (%s, %s)", app.AppName, app.Stage)
			}

			// A completed transition leaves no retry to own. A crashloop retry
			// that succeeded used to leave its loop entry behind (nothing
			// cleared it until the next fresh push), so a later failure resumed
			// at the old, already long backoff — and the observers, which now
			// defer to an active loop (driveCorrectedProdApp), would have
			// deferred to a loop with no goroutine behind it.
			am.clearCrashLoop(app.AppKey, app.Stage)

			// Verify if app has the latest requested state
			// TODO: properly handle it when verifying fails
			err := am.VerifyState(app)
			if err != nil {
				log.Error().Stack().Err(err).Msgf("failed to verify app state")
				return err
			}

			_ = am.UpdateTunnelState()

			return nil
		}

		// If anything goes wrong with the transition function
		// we should set the state change to FAILED
		// This will in turn update the in memory state and the local database state
		// which will in turn update the remote database as well
		if err != nil {
			setStateErr := am.StateObserver.Notify(app, common.FAILED)
			if setStateErr != nil {
				// wrap errors into one
				err = fmt.Errorf("failed to complete transition: %w", err)
			}
		}

		log.Error().Msgf("An error occured during transition from %s to %s for %s (%s)", fromState, payload.RequestedState, app.AppName, app.Stage)
		log.Error().Stack().Err(err).Msgf("The app state for %s (%s) has been set to FAILED", app.AppName, app.Stage)

		// Retry (crashloop) only failures that can heal on their own. A
		// deterministic configuration error reproduces identically on every
		// attempt — the observed field failure was 18+ retries over hours
		// against an invalid volume spec — so it ends here: the app stays
		// FAILED until a config change arrives as a fresh cloud push, which
		// re-drives it anyway.
		if shouldEnterCrashLoop(payload.Stage, err) {
			am.incrementCrashLoop(payload)
		} else if payload.Stage == common.PROD {
			am.clearCrashLoop(app.AppKey, app.Stage)
			log.Warn().Msgf("Transition failure for %s (%s) is a configuration error; not retrying", app.AppName, app.Stage)

			writeErr := am.StateMachine.LogManager.Write(payload.ContainerName.Prod,
				"This failure is caused by the app's configuration and will not be retried automatically. Update the app's configuration or release to recover.")
			if writeErr != nil {
				log.Debug().Err(writeErr).Msg("failed to write the configuration-error notice to the app log")
			}
		}
	}

	return nil
}

// HandleTransferFailure handles a failed file transfer by resetting the app state to REMOVED.
// This is called when the file transfer encounters an error (e.g., canceled, network failure).
func (am *AppManager) HandleTransferFailure(containerName string, transferErr error) {
	log.Warn().Err(transferErr).Str("container", containerName).Msg("File transfer failed, resetting app state")

	app := am.AppStore.GetAppByContainerName(containerName)
	if app == nil {
		log.Warn().Str("container", containerName).Msg("Could not find app for failed transfer, cannot reset state")
		return
	}

	app.StateLock.Lock()
	currentState := app.CurrentState
	app.StateLock.Unlock()

	// Only reset if the app is actually in TRANSFERING state
	if currentState != common.TRANSFERING {
		log.Debug().Str("container", containerName).Str("currentState", string(currentState)).Msg("App not in TRANSFERING state, skipping reset")
		return
	}

	// Notify the backend that the state should be REMOVED
	err := am.StateObserver.Notify(app, common.REMOVED)
	if err != nil {
		log.Error().Stack().Err(err).Str("container", containerName).Msg("Failed to notify backend of transfer failure")
	}
}

func IsInvalidOfflineTransition(app *common.App, payload common.TransitionPayload) bool {
	app.StateLock.Lock()
	defer app.StateLock.Unlock()

	notInstalled := app.CurrentState == common.REMOVED || app.CurrentState == common.UNINSTALLED
	buildRequest := app.RequestedState == common.BUILT
	removalRequest := payload.RequestedState == common.REMOVED || payload.RequestedState == common.UNINSTALLED

	if buildRequest {
		return true
	}

	// if the app is not on the device and we do any transition that would require internet we return true
	if notInstalled && payload.Stage == common.PROD && !removalRequest {
		return true
	}

	// cannot publish, update apps while offline
	if payload.RequestedState == common.PUBLISHED || (payload.RequestedState == common.PRESENT && payload.RequestUpdate) {
		return true
	}

	return false
}

// composeProjectLabel is the label docker compose stamps on every container it
// creates. Its value is the project name, which — because the agent runs
// compose without -p — is the normalized compose directory name, i.e.
// BuildComposeContainerName. It is the ONLY reliable way to tie a container
// back to a compose app.
const composeProjectLabel = "com.docker.compose.project"

func (am *AppManager) CleanupOrphanedContainers() error {
	if am.databaseUnsynced.Load() {
		log.Warn().Msg("Not cleaning up orphaned containers: the app database was created by this start and has not been synced with the backend yet")
		return nil
	}
	log.Info().Msg("🧹 Cleaning up orphaned containers not in database...")

	ctx := context.Background()
	containers, err := am.StateMachine.Container.GetContainers(ctx)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to list containers")
		return err
	}

	// Get all apps from database
	apps, err := am.AppStore.GetAllApps()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get apps from database")
		return err
	}

	// Two keep-sets, because a compose app is not one container. Compose names
	// its containers "<project>-<service>-<index>", so the project name never
	// appears as a container name — matching them by name marked EVERY service
	// container of EVERY compose app as an orphan (ParseContainerName happily
	// parses "prod_6_app_compose-nginx-1" as app "app_compose-nginx-1"), and
	// this ran on every WAMP reconnect: the app was force-removed and the state
	// correction right after it reinstalled the whole project — one full app
	// restart per reconnect. Compose apps are matched by project label only.
	expectedContainers := make(map[string]bool)
	expectedProjects := make(map[string]bool)
	for _, app := range apps {
		expectedContainers[common.BuildContainerName(app.Stage, app.AppKey, app.AppName)] = true
		expectedProjects[common.NormalizeComposeProjectName(
			common.BuildComposeContainerName(app.Stage, app.AppKey, app.AppName))] = true
	}

	// Check each container
	for _, cont := range containers {
		if project := cont.Labels[composeProjectLabel]; project != "" {
			if expectedProjects[common.NormalizeComposeProjectName(project)] {
				continue
			}

			// A project that does not follow the agent's naming scheme belongs
			// to someone else on this host (the appliance's own stack included)
			// and must never be touched.
			if _, _, _, err := common.ParseComposeContainerName(project); err != nil {
				continue
			}

			am.removeOrphanedContainer(ctx, cont.ID, project)
			continue
		}

		for _, name := range cont.Names {
			// Clean leading slash from container name
			cleanName := strings.TrimPrefix(name, "/")

			// A compose service container that somehow carries no project label
			// is left alone rather than mistaken for a legacy container: the
			// name parses, but the app it belongs to is keyed by project.
			if strings.Contains(cleanName, "_compose-") {
				continue
			}

			// Check if it matches our naming convention
			if _, _, _, err := common.ParseContainerName(cleanName); err != nil {
				continue
			}

			// If it's our container but not in database, remove it
			if !expectedContainers[cleanName] {
				am.removeOrphanedContainer(ctx, cont.ID, cleanName)
				break
			}
		}
	}

	log.Info().Msg("✨ Orphaned container cleanup complete")
	return nil
}

// removeOrphanedContainer force-removes one container and reports the outcome.
// A failure is logged and swallowed so one stuck container cannot abort the
// rest of the sweep.
func (am *AppManager) removeOrphanedContainer(ctx context.Context, containerID string, label string) {
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	log.Warn().
		Str("container", label).
		Str("id", shortID).
		Msg("❌ Removing orphaned container not in database")

	removeCtx, cancel := context.WithTimeout(ctx, time.Second*30)
	err := am.StateMachine.Container.RemoveContainerByID(removeCtx, containerID, map[string]interface{}{"force": true})
	cancel()

	if err != nil {
		log.Error().Stack().Err(err).
			Str("container", label).
			Msg("Failed to remove orphaned container")
		return
	}

	log.Info().Str("container", label).Msg("✅ Successfully removed orphaned container")
}

func (am *AppManager) CleanupOrphanedAppsFromDatabase(remotePayloads []common.TransitionPayload) error {
	log.Info().Msg("🧹 Syncing database with backend and cleaning up orphaned apps...")

	// Create a map of apps that should exist (from backend)
	expectedApps := make(map[string]bool)
	for _, payload := range remotePayloads {
		key := fmt.Sprintf("%d_%s", payload.AppKey, payload.Stage)
		expectedApps[key] = true
	}

	// Get all apps from local database
	localApps, err := am.AppStore.GetAllApps()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get apps from database")
		return err
	}

	ctx := context.Background()

	// Remove apps that are not in backend response
	for _, app := range localApps {
		key := fmt.Sprintf("%d_%s", app.AppKey, app.Stage)

		if !expectedApps[key] {
			log.Warn().
				Str("app", app.AppName).
				Uint64("app_key", app.AppKey).
				Str("stage", string(app.Stage)).
				Msg("❌ App not in backend sync, removing from database and Docker")

			// Remove container if it exists
			containerName := common.BuildContainerName(app.Stage, app.AppKey, app.AppName)
			getContainerCtx, cancel := context.WithTimeout(ctx, time.Second*10)
			cont, err := am.StateMachine.Container.GetContainer(getContainerCtx, containerName)
			cancel()

			if err == nil {
				removeCtx, cancel := context.WithTimeout(ctx, time.Second*30)
				err = am.StateMachine.Container.RemoveContainerByID(removeCtx, cont.ID, map[string]interface{}{"force": true})
				cancel()
				if err != nil && !errdefs.IsContainerNotFound(err) {
					log.Error().Stack().Err(err).Str("container", containerName).Msg("Failed to remove container")
				}
			}

			// Remove compose containers if they exist
			composeName := common.BuildComposeContainerName(app.Stage, app.AppKey, app.AppName)
			getComposeCtx, cancel := context.WithTimeout(ctx, time.Second*10)
			composeCont, err := am.StateMachine.Container.GetContainer(getComposeCtx, composeName)
			cancel()

			if err == nil {
				removeCtx, cancel := context.WithTimeout(ctx, time.Second*30)
				err = am.StateMachine.Container.RemoveContainerByID(removeCtx, composeCont.ID, map[string]interface{}{"force": true})
				cancel()
				if err != nil && !errdefs.IsContainerNotFound(err) {
					log.Error().Stack().Err(err).Str("container", composeName).Msg("Failed to remove compose container")
				}
			}

			// Remove images
			if app.Stage == common.DEV {
				imageName := common.BuildImageName(app.Stage, "amd64", app.AppKey, app.AppName) // Simplified - should get actual arch
				removeImageCtx, cancel := context.WithTimeout(ctx, time.Second*30)
				err = am.StateMachine.Container.RemoveImagesByName(removeImageCtx, imageName, map[string]interface{}{"force": true})
				cancel()
				if err != nil {
					log.Debug().Err(err).Str("image", imageName).Msg("Could not remove image (might not exist)")
				}
			} else {
				// For PROD, try to remove versioned images
				log.Debug().Str("app", app.AppName).Msg("Skipping image removal for PROD app (versioned images)")
			}

			// Remove from database
			err = am.AppStore.DeleteAppState(app.AppKey, app.Stage)
			if err != nil {
				log.Error().Stack().Err(err).Msg("Failed to delete app state from database")
			}

			err = am.AppStore.DeleteRequestedState(app.AppKey, app.Stage)
			if err != nil {
				log.Error().Stack().Err(err).Msg("Failed to delete requested state from database")
			}

			log.Info().
				Str("app", app.AppName).
				Uint64("app_key", app.AppKey).
				Str("stage", string(app.Stage)).
				Msg("✅ Successfully removed orphaned app")
		}
	}

	log.Info().Msg("✨ Database cleanup complete")
	return nil
}

func (am *AppManager) EnsureLocalRequestedStates() error {
	log.Debug().Msg("Ensuring local requested states")
	rStates, err := am.AppStore.GetRequestedStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get requested states")
		return err
	}

	// Stagger the startup to avoid overwhelming the backend
	for idx := range rStates {
		payload := rStates[idx]

		app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
		if err != nil {
			log.Error().Stack().Err(err).Msg("Failed to get app in EnsureLocalRequestedStates")
			return err
		}

		if app == nil {
			log.Warn().Msgf("no local app entry for requested app (%s, %s), skipping", payload.AppName, payload.Stage)
			continue
		}

		safe.Go(func() {
			app.StateLock.Lock()
			currentAppState := app.CurrentState
			app.StateLock.Unlock()

			// IsInvalidOfflineTransition skips heavy/online-only transitions on boot
			// (notably BUILT builds and PROD image pulls), so DEV apps reconcile but a
			// restart never triggers a build or download.
			if !IsInvalidOfflineTransition(app, payload) && currentAppState != payload.RequestedState {
				err := am.RequestAppState(payload)
				if err != nil {
					log.Error().Stack().Err(err).Msg("Failed to ensure local requested state")
				}
			}
		})

		// Add a small delay between starting each app to avoid overwhelming the backend
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

// hasPendingUpdate reports whether a requested-state row still carries an
// update that has not completed: the cloud requested one and the row's versions
// have not been collapsed by persistPostUpdateRequestedState yet. Mirrors the
// InitTransition update gate (which compares against the in-memory app.Version
// instead). Shared by VerifyState's re-drive, crashLoopWake's convergence check
// and the state observers' re-drive guard — reaching the target STATE does not
// finish an update, only the completed download does.
func hasPendingUpdate(payload common.TransitionPayload) bool {
	return payload.RequestUpdate &&
		payload.NewestVersion != payload.PresentVersion &&
		payload.RequestedState != common.UNINSTALLED
}

func (am *AppManager) VerifyState(app *common.App) error {
	log.Debug().Str("app", app.AppName).Msg("Verifying app state")
	log.Printf("Verifying if app (%s, %s) is in latest state...", app.AppName, app.Stage)

	requestedStatePayload, err := am.AppStore.GetRequestedState(app.AppKey, app.Stage)
	if err != nil {
		// A completed teardown deletes both database rows (state observer), so
		// a missing requested-state row right after reaching REMOVED or
		// UNINSTALLED is the expected end of a successful teardown — not a
		// verification failure.
		app.StateLock.Lock()
		verifyState := app.CurrentState
		app.StateLock.Unlock()
		if verifyState == common.REMOVED || verifyState == common.UNINSTALLED {
			log.Debug().Str("app", app.AppName).Msg("No requested state row after teardown; nothing to verify")
			return nil
		}

		log.Error().Stack().Err(err).Msg("Failed to get requested state in VerifyState")
		return err
	}

	log.Info().Msgf("Latest requested state (verify): %s", requestedStatePayload.RequestedState)

	app.StateLock.Lock()
	curAppState := app.CurrentState
	requestedState := app.RequestedState
	app.StateLock.Unlock()

	if curAppState == common.FAILED {
		log.Debug().Msg("App transition finished in a failed state")
		return nil
	}

	// A pending update whose target state already matches the current state (e.g.
	// an update requested while the app is RUNNING) would otherwise be missed:
	// the state-equality check below never re-drives it, so a request dropped
	// mid-transition (SecureTransition) is stranded until the next cloud push.
	// Re-drive it here. It is self-terminating: a successful update collapses
	// present==newest (so this is false next time), and a failed one lands in
	// FAILED (returned above).
	pendingUpdate := hasPendingUpdate(requestedStatePayload)

	if curAppState != requestedState || pendingUpdate {
		if pendingUpdate && curAppState == requestedState {
			log.Debug().Msgf("App (%s, %s) is in state %s but has a pending update (%s -> %s); re-driving...",
				app.AppName, app.Stage, curAppState, requestedStatePayload.PresentVersion, requestedStatePayload.NewestVersion)
		} else {
			log.Debug().Msgf("App (%s, %s) is not in latest state (%s), transitioning to %s...", app.AppName, app.Stage, curAppState, requestedState)
		}

		// transition again
		safe.Go(func() {
			_ = am.RequestAppState(requestedStatePayload)
		})
	} else {
		// Re-materialise the credential files for an app that needs no
		// transition — covers an epoch bump (rotation) that happened while
		// the device was offline.
		refreshAppCredentialEnvFiles(
			am.StateMachine.Container.GetConfig(),
			am.StateMachine.AppCredKey(),
			requestedStatePayload,
		)

		err = am.syncPortState(requestedStatePayload, app)
		if err != nil {
			log.Error().Stack().Err(err).Msgf("failed to sync port state")
			return err
		}
	}

	if curAppState == common.BUILT && requestedState == common.BUILT {
		// The build has finished and should now be put to PRESENT
		err = am.StateObserver.Notify(app, common.PRESENT)
		if err != nil {
			log.Error().Stack().Err(err).Msg("Failed to notify state observer for PRESENT state")
			return err
		}
	}

	return nil
}

func (am *AppManager) UpdateCurrentAppState(payload common.TransitionPayload) error {
	log.Debug().Str("app", payload.AppName).Msg("UpdateCurrentAppState called")
	app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get app in UpdateCurrentAppState")
		return err
	}

	if app == nil {
		return fmt.Errorf("app (%d, %s) does not exist in the local app store", payload.AppKey, payload.Stage)
	}

	app.StateLock.Lock()

	curAppState := app.CurrentState

	if curAppState == common.BUILT ||
		curAppState == common.PUBLISHED ||
		(curAppState == common.PRESENT && app.RequestedState == common.BUILT) {
		if payload.CurrentState != "" {
			app.CurrentState = payload.CurrentState
		}
	}

	if payload.PresentVersion != "" {
		app.Version = payload.PresentVersion
	}

	// The backend owns the name behind an app key. A local entry left over from
	// another backend (a reset or a re-registered device reusing the key) keeps
	// the old name otherwise, so the observer looks for a container that does
	// not exist and every restart's orphan sweep removes the real one.
	if payload.AppName != "" && payload.AppName != app.AppName {
		log.Info().Str("from", app.AppName).Str("to", payload.AppName).Uint64("app_key", app.AppKey).
			Str("stage", string(app.Stage)).Msg("Backend renamed the app behind this key, adopting the new name")
		app.AppName = payload.AppName
	}

	app.StateLock.Unlock()

	log.Debug().Str("app", payload.AppName).Msg("Updating local app state")
	return am.AppStore.UpdateLocalAppState(app, curAppState)
}

func (am *AppManager) CreateOrUpdateApp(payload common.TransitionPayload) error {
	log.Debug().Str("app", payload.AppName).Msg("CreateOrUpdateApp called")
	app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get app in CreateOrUpdateApp")
		return err
	}

	payload.RequestedState = common.TransientToActualState(payload.RequestedState)

	if app == nil {
		app, err = am.AppStore.AddApp(payload)
		if err != nil {
			log.Error().Stack().Err(err).Msg("Failed to add app in CreateOrUpdateApp")
			return err
		}
	}

	app.StateLock.Lock()

	// A BUILT app keeps its requested state (the build auto-progresses to PRESENT),
	// but explicit teardown must still be honoured so it can be uninstalled/removed.
	isTeardown := payload.RequestedState == common.UNINSTALLED || payload.RequestedState == common.REMOVED
	if app.CurrentState == common.BUILT && app.RequestedState != common.BUILT && !isTeardown {
		app.StateLock.Unlock()
		log.Debug().Str("app", payload.AppName).Msg("Skipping update of requestedState as app is already built")
		return nil
	}

	app.RequestedState = payload.RequestedState

	app.StateLock.Unlock()

	log.Debug().Str("app", payload.AppName).Msg("Updating local requested app state")
	return am.AppStore.UpdateLocalRequestedState(payload)
}

// EnsureRemoteRequestedStates iterates over all requested states found in the local database, and transitions were neccessary.
func (am *AppManager) EnsureRemoteRequestedStates() error {
	log.Debug().Msg("Ensuring remote requested states")
	payloads, err := am.AppStore.GetRequestedStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to get requested states in EnsureRemoteRequestedStates")
		return err
	}

	for i := range payloads {
		payload := payloads[i]

		// Do not auto-reconcile DEV apps on reconnect (they are driven by the
		// developer's interactive actions) unless a stop never arrived, nor redo
		// one-shot publishes/builds.
		if payload.Stage == common.DEV {
			app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
			if err != nil || app == nil {
				continue
			}
			app.StateLock.Lock()
			currentState := app.CurrentState
			app.StateLock.Unlock()
			if !devStopOutstanding(payload.RequestedState, currentState) {
				log.Debug().Str("app", payload.AppName).Msg("Skipping reconnect reconcile for DEV")
				continue
			}
		} else if payload.RequestedState == common.PUBLISHED || payload.RequestedState == common.BUILT {
			log.Debug().Str("app", payload.AppName).Msg("Skipping reconnect reconcile for PUBLISHED/BUILT")
			continue
		}

		safe.Go(func() {
			log.Debug().Str("app", payload.AppName).Msg("Requesting app state in goroutine")
			_ = am.RequestAppState(payload)
		})
	}

	return nil
}

// UpdateLocalRequestedAppStatesWithRemote is responsible for updating the local database with fetched requested states.
// The local database will be updated with the fetched requested states. In case an app state does not exist yet locally, one will be created.
func (am *AppManager) UpdateLocalRequestedAppStatesWithRemote(newestPayloads []common.TransitionPayload) error {
	log.Debug().Msg("Updating local requested app states with remote")

	log.Info().Msgf("Found %d app states, updating local database with new requested states..", len(newestPayloads))
	for i := range newestPayloads {
		payload := newestPayloads[i]

		// PROD apps are reconciled from the bulk sync. DEV apps are driven by the
		// developer's interactive actions, not overwritten from the cloud sync
		// (overwriting them races an active dev session: build/run/stop), except
		// for a stop that never arrived (devStopOutstanding).
		if payload.Stage == common.PROD {
			err := am.CreateOrUpdateApp(payload)
			if err != nil {
				return err
			}
			continue
		}

		app, err := am.AppStore.GetApp(payload.AppKey, payload.Stage)
		if err != nil {
			return err
		}
		if app == nil {
			continue
		}
		app.StateLock.Lock()
		currentState, requestedState := app.CurrentState, app.RequestedState
		app.StateLock.Unlock()

		if requestedState != payload.RequestedState && devStopOutstanding(payload.RequestedState, currentState) {
			log.Info().Str("app", payload.AppName).Msgf("Applying %s requested in the cloud to the running DEV app", payload.RequestedState)
			err := am.CreateOrUpdateApp(payload)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// devStopOutstanding reports whether a DEV app still runs although the
// requested state is a stop or teardown. The cloud records a DEV stop before
// pushing it, and the push is dropped while the device is offline; since DEV
// apps are otherwise not reconciled on reconnect, the container would keep
// running and every further stop would find nothing to change in the cloud.
func devStopOutstanding(requestedState, currentState common.AppState) bool {
	switch requestedState {
	case common.PRESENT, common.REMOVED, common.UNINSTALLED:
		return currentState == common.RUNNING
	}
	return false
}

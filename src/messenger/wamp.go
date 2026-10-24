package messenger

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/diskguard"
	"reagent/errdefs"
	"reagent/messenger/topics"
	"reagent/migratestate"

	"github.com/ironflock/nexus/v3/client"
	"github.com/ironflock/nexus/v3/transport"
	"github.com/ironflock/nexus/v3/wamp"
	"github.com/ironflock/nexus/v3/wamp/crsign"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// reconnectBackoff is the delay before retrying something that is NOT a failed
// dial: a local connect-config error, and the duplicate-serial re-register
// race further down. Kept short and fixed — maxDuplicateSerialAttempts is
// calibrated against it, and neither case puts a connection attempt on the
// wire.
const reconnectBackoff = time.Second

// A failed DIAL backs off exponentially instead, doubling from
// reconnectBackoffBase up to reconnectBackoffMax.
//
// The old fixed one-second delay had the agent opening ~27 connections a
// minute for as long as an outage lasted, each from a fresh source port and
// each abandoned mid-handshake. A corporate IPS reads that as a port scan:
// at tls-sf015 on 2026-09-10, new TCP connections from an edge PC to the
// appliance were silently dropped for minutes at a time while established
// ones kept flowing, and the agent's own retry storm is a plausible trigger
// for the block that sustained it. At the cap the agent tries three times a
// minute instead, and still recovers within 20s of the path clearing.
const (
	reconnectBackoffBase = time.Second
	reconnectBackoffMax  = 20 * time.Second

	// reconnectBackoffJitter spreads each delay by this fraction either way,
	// so that a fleet reconnecting after an appliance router restart does not
	// arrive in lockstep.
	reconnectBackoffJitter = 0.2
)

// dialBackoff returns how long to wait before the next dial, given how many
// consecutive dials have failed (1 for the first failure). The result is
// always positive and never exceeds reconnectBackoffMax plus its jitter.
func dialBackoff(consecutiveFailures int) time.Duration {
	shift := consecutiveFailures - 1
	if shift < 0 {
		shift = 0
	}
	// Bounded well below the point where the shift would overflow the
	// int64 nanoseconds behind a Duration.
	if shift > 30 {
		shift = 30
	}

	delay := reconnectBackoffBase << shift
	if delay > reconnectBackoffMax || delay <= 0 {
		delay = reconnectBackoffMax
	}

	spread := float64(delay) * reconnectBackoffJitter
	return time.Duration(float64(delay) + (rand.Float64()*2-1)*spread) //nolint:gosec // G404: jitter for reconnect delays, not security relevant
}

// maxDuplicateSerialAttempts bounds how many consecutive reconnects may hit
// ProcedureAlreadyExists when registering the connection-established procedure
// before we conclude a second device is genuinely using this serial. After a
// transient drop (e.g. keepalive stalled by a log flood) the router may not
// have reaped our previous session yet, so the re-register races its teardown.
// Tolerating a few attempts (~maxDuplicateSerialAttempts * reconnectBackoff)
// lets that clear without restarting the agent, while a real duplicate serial
// — which never clears — still trips the exit.
const maxDuplicateSerialAttempts = 8

// setupTestamentTimeout bounds the testament-installation Call so a stuck
// router can never wedge the reconnect path.
const setupTestamentTimeout = 10 * time.Second

// cancelStreamsTimeout bounds the synchronous container-cleanup step that
// runs before a reconnect attempt. If Docker is stuck, we still reconnect.
const cancelStreamsTimeout = 5 * time.Second

// DefaultHeartbeatInterval is the interval between application-level heartbeats
// (a device-status update that doubles as a liveness probe + periodic stats
// report) used when SocketConfig.HeartbeatInterval is zero.
const DefaultHeartbeatInterval = 30 * time.Second

type WampSession struct {
	client         NexusClient
	container      container.Container
	agentConfig    *config.Config
	socketConfig   *SocketConfig
	clientProvider ClientProvider // nil means use default client.ConnectNet
	onConnect      func(reconnect bool)
	heartbeatDone  chan struct{} // closed by the heartbeat when it detects a dead connection; re-created per connect()
	// tunnelCapableFn reports per-device tunnel capability; injected by the
	// agent so the heartbeat can carry it to the UI without a dedicated RPC.
	// Nil until wired (the field is then omitted from the payload).
	tunnelCapableFn func() bool
	// dockerAvailableFn reports whether the Docker daemon currently answers;
	// injected by the agent (same pattern as tunnelCapableFn) so the heartbeat
	// carries live daemon health to the UI. Nil until wired (the field is then
	// omitted from the payload).
	dockerAvailableFn func() bool

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	// statusSlot serializes UpdateRemoteDeviceStatus end to end (probe +
	// send). The heartbeat, OnConnect and the daemon-recovery transition
	// pushes all call it concurrently; without it a push whose
	// docker_available probe hung across a daemon recovery could send its
	// stale payload AFTER a fresher push, and the backend persists in arrival
	// order — leaving a wrong badge for up to a heartbeat.
	//
	// One slot per connection, re-created in connect(): a push that never
	// returns (a call parked on a dead writer, a probe hung on a dead mount)
	// keeps only the slot of the connection it started on, so the next
	// connection's pushes are not queued behind it. Waiting for the slot is
	// bounded by statusUpdateTimeout for the same reason.
	statusSlot          chan struct{}
	statusUpdateTimeout time.Duration
	// lastStatusPush is when a push last released the slot (unix nanos);
	// the stall watchdog reads it as its sign of progress.
	lastStatusPush atomic.Int64
	// onStall is what the watchdog does about a heartbeat that stopped
	// completing rounds: Reconnect, unless a test injects something else.
	onStall func()
	// failureLogPeriod is the period of every registration's and
	// subscription's failure log (see failureLog); zero means
	// defaultFailureLogPeriod. A test shortens it.
	failureLogPeriod time.Duration
}

// ErrStatusUpdateStuck is returned when a status push cannot get its turn
// within statusUpdateTimeout: the push holding the slot has outlived every
// timeout that should have bounded it. The caller is not queued behind it;
// the stall watchdog decides what happens next.
var ErrStatusUpdateStuck = errors.New("previous device status update still in flight")

// heartbeatStallRounds is how many consecutive heartbeat intervals may pass
// on a live connection without any status push completing before the
// connection counts as stalled.
const heartbeatStallRounds = 3

// SetTunnelCapableFunc wires the per-device tunnel-capability getter into the
// heartbeat payload. Called once by the agent after construction.
func (s *WampSession) SetTunnelCapableFunc(fn func() bool) {
	s.tunnelCapableFn = fn
}

// SetDockerAvailableFunc wires the Docker daemon health getter into the
// heartbeat payload. Called once by the agent after construction.
func (s *WampSession) SetDockerAvailableFunc(fn func() bool) {
	s.dockerAvailableFn = fn
}

type DeviceStatus string

const (
	CONNECTED    DeviceStatus = "CONNECTED"
	DISCONNECTED DeviceStatus = "DISCONNECTED"
	CONFIGURING  DeviceStatus = "CONFIGURING"
	// EMERGENCY is reported in place of CONNECTED while the device is critically
	// low on disk (see package diskguard): it is stopping non-platform containers
	// and refusing new app start/build/download.
	EMERGENCY DeviceStatus = "EMERGENCY"
)

var ErrNotConnected = errors.New("not connected")

// wampLogger adapts a zerolog.Logger to the nexus stdlog.StdLog interface
// so the nexus client/router can write through our structured logger.
type wampLogger struct {
	logger *zerolog.Logger
}

func (l wampLogger) Print(v ...interface{})                 { l.logger.Print(v...) }
func (l wampLogger) Println(v ...interface{})               { l.logger.Print(v...) }
func (l wampLogger) Printf(format string, v ...interface{}) { l.logger.Printf(format, v...) }

type SocketConfig struct {
	PingPongTimeout   time.Duration
	ResponseTimeout   time.Duration
	ConnectionTimeout time.Duration
	HeartbeatInterval time.Duration // Default: 30s (DefaultHeartbeatInterval) if zero
	SetupTestament    bool
}

// ClientProvider is a function that attempts to connect and returns a NexusClient.
// Used to inject a fake client in tests.
type ClientProvider func(ctx context.Context, url string, cfg client.Config) (NexusClient, error)

func createConnectConfig(cfg *config.Config, socketConfig *SocketConfig) (*client.Config, error) {
	reswarmConfig := cfg.ReswarmConfig

	clientCfg := client.Config{
		Realm: "realm1",
		HelloDetails: wamp.Dict{
			"authid": fmt.Sprintf("%d-%d", reswarmConfig.SwarmKey, reswarmConfig.DeviceKey),
		},
		AuthHandlers: map[string]client.AuthFunc{
			"wampcra": clientAuthFunc(reswarmConfig.Secret),
		},
		Debug:  cfg.CommandLineArguments.DebugMessaging,
		Logger: wampLogger{logger: &log.Logger},
	}

	if socketConfig.PingPongTimeout != 0 {
		clientCfg.WsCfg = transport.WebsocketConfig{
			KeepAlive: socketConfig.PingPongTimeout,
		}
	}

	if socketConfig.ResponseTimeout != 0 {
		clientCfg.ResponseTimeout = socketConfig.ResponseTimeout
	}

	return &clientCfg, nil
}

// SetOnConnect sets the callback invoked after each successful connection or
// reconnection. The callback receives a flag indicating whether this is a
// reconnect (true) or the initial connection (false).
func (s *WampSession) SetOnConnect(cb func(reconnect bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onConnect = cb
}

// NewWampSession creates a new WampSession and establishes the initial WAMP
// connection. If clientProvider is nil, the default client.ConnectNet is used.
func NewWampSession(cfg *config.Config, socketConfig *SocketConfig, container container.Container, clientProvider ClientProvider) (*WampSession, error) {
	ctx, cancel := context.WithCancel(context.Background())

	session := &WampSession{
		agentConfig:    cfg,
		socketConfig:   socketConfig,
		container:      container,
		clientProvider: clientProvider,
		ctx:            ctx,
		cancel:         cancel,
	}
	// One heartbeat interval is well above a push's legitimate worst case
	// (3s docker probe + 10s call + the cancel round-trip).
	session.statusUpdateTimeout = session.heartbeatInterval()
	session.onStall = session.Reconnect

	if err := session.connect(false); err != nil {
		cancel()
		return nil, err
	}

	go session.watchHeartbeat()

	return session, nil
}

func (s *WampSession) heartbeatInterval() time.Duration {
	if s.socketConfig.HeartbeatInterval != 0 {
		return s.socketConfig.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

// connect dials the router (with retry until success or session close) and
// installs the new client plus its disconnect watcher. When isReconnect is
// true, the onConnect callback is invoked.
//
// Liveness contract: the only error returned is the one from dial() when the
// session context is cancelled (i.e. Close() was called). Any other failure
// during post-dial setup is logged and recovered: the watcher is always
// spawned so a subsequent disconnect drives another reconnect cycle.
func (s *WampSession) connect(isReconnect bool) error {
	c, err := s.dial()
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.client = c
	cb := s.onConnect
	// Fresh per-connection heartbeat channel: the heartbeat closes it on
	// repeated failures and the watcher treats that as a lost connection.
	hbDone := make(chan struct{})
	s.heartbeatDone = hbDone
	s.statusSlot = make(chan struct{}, 1)
	s.mu.Unlock()

	if s.socketConfig.SetupTestament {
		if testErr := s.setupTestamentBounded(); testErr != nil {
			if isReconnect {
				log.Error().Err(testErr).Msg("failed to setup testament after reconnect — continuing; watcher will reconnect on next disconnect")
			} else {
				// Initial connect: caller decides what to do. Do not leak
				// the dialled client.
				_ = c.Close()
				s.mu.Lock()
				s.client = nil
				s.mu.Unlock()
				return testErr
			}
		}
	}

	s.spawnWatcher(c, hbDone)
	s.startHeartbeat(hbDone)

	if isReconnect && cb != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Msgf("Panic in onConnect callback (recovered, agent stays online): %+v\n%s", r, debug.Stack())
				}
			}()
			log.Info().Msg("Re-initializing after reconnection...")
			cb(true)
			log.Info().Msg("Successfully re-initialized after reconnection")
		}()
	}

	return nil
}

// spawnWatcher starts watchDisconnect in its own goroutine with a panic
// recovery wrapper that re-spawns the watcher on panic so the reconnect
// machinery survives unexpected failures.
func (s *WampSession) spawnWatcher(c NexusClient, hbDone chan struct{}) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Msgf("Panic in disconnect watcher (recovered): %+v\n%s", r, debug.Stack())
				// A panic here would otherwise leave the session with no
				// liveness monitor. Re-arm by triggering a reconnect cycle
				// directly so the agent stays self-healing.
				if s.ctx.Err() == nil {
					if connErr := s.connect(true); connErr != nil {
						log.Warn().Err(connErr).Msg("reconnect aborted after watcher panic")
					}
				}
			}
		}()
		s.watchDisconnect(c, hbDone)
	}()
}

// watchDisconnect waits on the underlying client's Done channel OR the
// heartbeat's failure signal and triggers a reconnect when either fires.
// Exits silently if the session is closed.
func (s *WampSession) watchDisconnect(c NexusClient, hbDone chan struct{}) {
	select {
	case <-c.Done():
		log.Warn().Msg("WAMP connection lost, reconnecting...")
	case <-hbDone:
		log.Warn().Msg("Heartbeat detected connection failure, reconnecting...")
		// The transport never noticed, so nothing has cancelled this
		// client: close it in the background so a request parked on its
		// writer is released. Not awaited — Close waits for running
		// invocation handlers.
		go func() { _ = c.Close() }()
	case <-s.ctx.Done():
		return
	}

	s.cancelContainerStreams()

	if err := s.connect(true); err != nil {
		log.Warn().Err(err).Msg("reconnect aborted")
	}
}

// watchHeartbeat is the backstop for a heartbeat that stops completing
// rounds while the connection looks fine — a push parked on a dead writer,
// a probe hung on a dead mount, a heartbeat goroutine that died. None of
// that fails a call or wakes the disconnect watcher, so from outside the
// device just stays DISCONNECTED (tls-sf015, 2026-09-16: eight hours, until
// someone restarted the agent). After heartbeatStallRounds intervals with
// no push released it runs onStall — a reconnect, which brings a fresh slot
// and a fresh heartbeat and leaves whatever is stuck behind.
func (s *WampSession) watchHeartbeat() {
	interval := s.heartbeatInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastSeen := s.lastStatusPush.Load()
	stale := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}

		if !s.Connected() {
			// The disconnect watcher owns this phase; its dial loop is bounded.
			stale = 0
			continue
		}
		if seen := s.lastStatusPush.Load(); seen != lastSeen {
			lastSeen = seen
			stale = 0
			continue
		}
		stale++
		if stale < heartbeatStallRounds {
			continue
		}
		log.Error().Msgf("No device status push completed in %d heartbeat intervals while connected, forcing a reconnect", stale)
		stale = 0
		s.onStall()
	}
}

// acquireStatusSlot takes the current connection's status slot, waiting at
// most statusUpdateTimeout. The returned release stamps lastStatusPush so
// the stall watchdog sees progress.
func (s *WampSession) acquireStatusSlot() (release func(), err error) {
	s.mu.Lock()
	slot := s.statusSlot
	timeout := s.statusUpdateTimeout
	s.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case slot <- struct{}{}:
	case <-t.C:
		return nil, ErrStatusUpdateStuck
	}
	return func() {
		s.lastStatusPush.Store(time.Now().UnixNano())
		<-slot
	}, nil
}

// startHeartbeat periodically sends a device-status update (CONNECTED) that
// doubles as an application-level liveness probe and a periodic stats report.
// After maxConsecutiveFailures failed sends it closes hbDone, which the
// disconnect watcher treats as a lost connection and uses to drive a reconnect.
// hbDone is the per-connection channel created in connect(); once a newer
// connection replaces s.heartbeatDone this goroutine exits, so heartbeats never
// accumulate across reconnects.
func (s *WampSession) startHeartbeat(hbDone chan struct{}) {
	heartbeatInterval := s.heartbeatInterval()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Msgf("Panic in heartbeat (recovered): %+v\n%s", r, debug.Stack())
			}
		}()

		consecutiveFailures := 0
		const maxConsecutiveFailures = 2 // trigger reconnect after 2 failures

		for {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(heartbeatInterval):
			}

			// A newer connection has taken over (e.g. after a transport-level
			// reconnect): stop this heartbeat so only one runs at a time.
			s.mu.Lock()
			current := s.heartbeatDone
			s.mu.Unlock()
			if current != hbDone {
				return
			}

			if !s.Connected() {
				log.Warn().Msg("Connection lost detected in heartbeat, waiting for reconnection...")
				consecutiveFailures = 0
				continue
			}

			if err := s.UpdateRemoteDeviceStatus(CONNECTED); errors.Is(err, ErrStatusUpdateStuck) {
				// Not a connection failure: a reconnect driven from here would
				// not free the stuck push. The stall watchdog handles it.
				log.Warn().Err(err).Msg("Heartbeat skipped")
			} else if err != nil {
				consecutiveFailures++
				log.Warn().Err(err).Msgf("Failed to send heartbeat (%d/%d failures), connection may be lost", consecutiveFailures, maxConsecutiveFailures)
				if consecutiveFailures >= maxConsecutiveFailures {
					log.Error().Msg("Connection appears to be broken (multiple heartbeat failures), signaling reconnection...")
					close(hbDone)
					return
				}
			} else {
				if consecutiveFailures > 0 {
					log.Info().Msg("Heartbeat successful, connection restored")
				}
				consecutiveFailures = 0
			}
		}
	}()

	log.Debug().Msg("Messenger: Started heartbeat")
}

// cancelContainerStreams runs container.CancelAllStreams in a bounded window
// so a stuck Docker daemon cannot block reconnect.
func (s *WampSession) cancelContainerStreams() {
	if s.container == nil {
		return
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic in CancelAllStreams: %+v", r)
			}
		}()
		done <- s.container.CancelAllStreams()
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Error().Err(err).Msg("error cancelling container streams during reconnect")
		}
	case <-time.After(cancelStreamsTimeout):
		log.Warn().Dur("timeout", cancelStreamsTimeout).Msg("container.CancelAllStreams timed out — proceeding with reconnect anyway")
	}
}

// setupTestamentBounded wraps SetupTestament with a hard timeout so the
// testament Call cannot hang the reconnect path even if nexus's response
// timeout is misconfigured or the router is unresponsive.
func (s *WampSession) setupTestamentBounded() error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic in SetupTestament: %+v", r)
			}
		}()
		done <- s.SetupTestament()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(setupTestamentTimeout):
		return fmt.Errorf("SetupTestament timed out after %s", setupTestamentTimeout)
	}
}

// dial repeatedly attempts to establish a WAMP connection until one succeeds
// or the session context is cancelled. Returns the connected client, or
// context.Canceled if the session is closed during dialling.
func (s *WampSession) dial() (NexusClient, error) {
	if s.agentConfig.CommandLineArguments.Offline {
		log.Warn().Msg("Started in offline mode, will not establish a socket connection!")
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}

	log.Debug().Msg("Attempting to establish a socket connection...")

	dupRegisterFailures := 0
	dialFailures := 0
	for attempt := 1; ; attempt++ {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}

		connectionConfig, err := createConnectConfig(s.agentConfig, s.socketConfig)
		if err != nil {
			log.Error().Err(err).Msg("failed to create connect config")
			if !s.sleepOrDone(reconnectBackoff) {
				return nil, s.ctx.Err()
			}
			continue
		}

		dialCtx := s.ctx
		var dialCancel context.CancelFunc
		if s.socketConfig.ConnectionTimeout != 0 {
			dialCtx, dialCancel = context.WithTimeout(s.ctx, s.socketConfig.ConnectionTimeout)
		}

		requestStart := time.Now()
		var c NexusClient
		if s.clientProvider != nil {
			c, err = s.clientProvider(dialCtx, s.agentConfig.ReswarmConfig.DeviceEndpointURL, *connectionConfig)
		} else {
			c, err = client.ConnectNet(dialCtx, s.agentConfig.ReswarmConfig.DeviceEndpointURL, *connectionConfig)
		}
		if dialCancel != nil {
			dialCancel()
		}

		if err != nil {
			if strings.Contains(err.Error(), "WAMP-CRA client signature is invalid") {
				fmt.Println("The IronFlock device connect authentication failed")
				os.Exit(1)
			}
			dialFailures++
			backoff := dialBackoff(dialFailures)
			log.Debug().Err(err).Msgf("Failed to establish a websocket connection (duration: %s, attempt #%d), reattempting in %s", time.Since(requestStart), attempt, backoff)
			if !s.sleepOrDone(backoff) {
				return nil, s.ctx.Err()
			}
			continue
		}

		// The transport is up; the next failure starts the ramp again.
		dialFailures = 0

		// Register the device's "I am here" procedure. A second device with
		// the same serial trying to register the same procedure is rejected
		// by the router with ProcedureAlreadyExists, which we treat as a
		// fatal duplicate-serial condition.
		topic := common.BuildExternalApiTopic(s.agentConfig.ReswarmConfig.SerialNumber, "wamp_connection_established")
		invokeHandler := func(ctx context.Context, _ *wamp.Invocation) client.InvokeResult {
			return client.InvokeResult{Args: wamp.List{"Hello :-)"}}
		}
		if regErr := c.Register(topic, invokeHandler, nil); regErr != nil {
			if strings.Contains(regErr.Error(), string(wamp.ErrProcedureAlreadyExists)) {
				// Usually our OWN previous session hasn't been reaped by the
				// router yet after a transient drop (e.g. a log flood stalled
				// keepalive, the router dropped us, and we reconnected before it
				// cleaned up). That race must NOT restart the agent. Only treat
				// it as a genuine duplicate serial — a second physical device on
				// this serial — once it persists across several reconnects,
				// because that condition never clears on its own.
				dupRegisterFailures++
				if dupRegisterFailures >= maxDuplicateSerialAttempts {
					fmt.Printf("a WAMP connection for %s already exists\n", s.agentConfig.ReswarmConfig.SerialNumber)
					os.Exit(1)
				}
				log.Warn().Err(regErr).Msgf("connection-established Register hit ProcedureAlreadyExists (%d/%d); old session likely not yet reaped, retrying", dupRegisterFailures, maxDuplicateSerialAttempts)
				_ = c.Close()
				if !s.sleepOrDone(reconnectBackoff) {
					return nil, s.ctx.Err()
				}
				continue
			}
			// Non-duplicate Register failure: log it so we can debug, but
			// keep the client. If the connection is genuinely dead the
			// watcher's Done() will fire and trigger another reconnect.
			log.Debug().Err(regErr).Msg("connection-established Register failed (non-fatal)")
		}
		log.Debug().Msgf("Successfully established a connection (duration: %s)", time.Since(requestStart))
		return c, nil
	}
}

// sleepOrDone sleeps for d, returning false if the session context is
// cancelled before the sleep completes.
func (s *WampSession) sleepOrDone(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *WampSession) Publish(topic topics.Topic, args []interface{}, kwargs common.Dict, options common.Dict) error {
	c := s.currentClient()
	if c == nil {
		return ErrNotConnected
	}

	if err := c.Publish(string(topic), wamp.Dict(options), args, wamp.Dict(kwargs)); err != nil {
		log.Debug().Err(err).Str("topic", string(topic)).Msg("Failed to publish to topic")
		return err
	}
	return nil
}

func (s *WampSession) Connected() bool {
	c := s.currentClient()
	if c == nil {
		return false
	}
	return c.Connected()
}

// Client returns the underlying NexusClient. Primarily useful for tests.
func (s *WampSession) Client() NexusClient {
	return s.currentClient()
}

func (s *WampSession) currentClient() NexusClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// A failure log lets failureLogBurst lines through per period, by default
// defaultFailureLogPeriod, and counts the rest.
const (
	failureLogBurst         = 5
	defaultFailureLogPeriod = time.Minute
)

// failureLog samples the log lines of one registration's or subscription's
// failures. Whoever may call the procedure or publish to the topic decides how
// often it fails, and on a released router that is anyone: logging every
// failure would let them flood the device's log and rotate its history away.
// The lines it drops are counted, and the count is logged once the period is
// over, so a flood still shows.
type failureLog struct {
	sampler zerolog.BurstSampler
	what    string // what failed, for the count: "refused calls of <topic>"

	mu         sync.Mutex
	suppressed int
}

func (s *WampSession) newFailureLog(what string) *failureLog {
	period := s.failureLogPeriod
	if period == 0 {
		period = defaultFailureLogPeriod
	}

	return &failureLog{
		sampler: zerolog.BurstSampler{Burst: failureLogBurst, Period: period},
		what:    what,
	}
}

// Sample implements zerolog.Sampler.
func (l *failureLog) Sample(lvl zerolog.Level) bool {
	if l.sampler.Sample(lvl) {
		return true
	}

	l.mu.Lock()
	l.suppressed++
	first := l.suppressed == 1
	l.mu.Unlock()

	// The first line dropped since the last count schedules the next one, so
	// an idle registration keeps no timer.
	if first {
		time.AfterFunc(l.sampler.Period, l.logSuppressed)
	}

	return false
}

func (l *failureLog) logSuppressed() {
	l.mu.Lock()
	suppressed := l.suppressed
	l.suppressed = 0
	l.mu.Unlock()

	log.Warn().Int("suppressed", suppressed).Msgf("%d more %s were not logged in the last %s", suppressed, l.what, l.sampler.Period)
}

// logger is the global logger, sampled by l. It is taken per line so a line
// goes wherever the global logger points by then.
func (l *failureLog) logger() *zerolog.Logger {
	sampled := log.Sample(l)
	return &sampled
}

func (s *WampSession) Subscribe(topic topics.Topic, cb func(Result) error, options common.Dict) error {
	c := s.currentClient()
	if c == nil {
		return ErrNotConnected
	}

	// The callback runs once per event, so whoever may publish to the topic
	// decides how often it fails: a terminal refuses every keystroke from
	// someone who does not own it.
	failed := s.newFailureLog(fmt.Sprintf("failed events of %s", topic))

	handler := func(event *wamp.Event) {
		// The client runs event handlers on its receive loop and recovers
		// none of them: a callback that panics on a malformed event would
		// take the agent down with it.
		defer func() {
			if r := recover(); r != nil {
				if e := failed.logger().Error(); e.Enabled() {
					e.Msgf("Recovered a panic during the subscribe result of %s: %v\n%s", topic, r, debug.Stack())
				}
			}
		}()

		cbEventMap := Result{
			Subscription: uint64(event.Subscription),
			Publication:  uint64(event.Publication),
			Details:      common.Dict(event.Details),
			Arguments:    []interface{}(event.Arguments),
			ArgumentsKw:  common.Dict(event.ArgumentsKw),
		}
		if err := cb(cbEventMap); err != nil {
			failed.logger().Error().Stack().Err(err).Msgf("An error occured during the subscribe result of %s", topic)
		}
	}

	return c.Subscribe(string(topic), handler, wamp.Dict(options))
}

func (s *WampSession) GetConfig() *config.Config {
	return s.agentConfig
}

func (s *WampSession) SubscriptionID(topic topics.Topic) (uint64, bool) {
	c := s.currentClient()
	if c == nil {
		return 0, false
	}
	subID, ok := c.SubscriptionID(string(topic))
	return uint64(subID), ok
}

func (s *WampSession) RegistrationID(topic topics.Topic) (uint64, bool) {
	c := s.currentClient()
	if c == nil {
		return 0, false
	}
	regID, ok := c.RegistrationID(string(topic))
	return uint64(regID), ok
}

func (s *WampSession) Call(
	ctx context.Context,
	topic topics.Topic,
	args []interface{},
	kwargs common.Dict,
	options common.Dict,
	progCb func(Result),
) (Result, error) {
	c := s.currentClient()
	if c == nil {
		return Result{}, ErrNotConnected
	}

	var handler func(result *wamp.Result)
	if progCb != nil {
		handler = func(result *wamp.Result) {
			progCb(Result{
				Request:     uint64(result.Request),
				Details:     common.Dict(result.Details),
				Arguments:   []interface{}(result.Arguments),
				ArgumentsKw: common.Dict(result.ArgumentsKw),
			})
		}
	}

	callResult, err := c.Call(ctx, string(topic), wamp.Dict(options), args, wamp.Dict(kwargs), handler)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Request:     uint64(callResult.Request),
		Details:     common.Dict(callResult.Details),
		Arguments:   []interface{}(callResult.Arguments),
		ArgumentsKw: common.Dict(callResult.ArgumentsKw),
	}, nil
}

func (s *WampSession) GetSessionID() uint64 {
	c := s.currentClient()
	if c == nil {
		return 0
	}
	return uint64(c.ID())
}

// errHandlerPanicked answers a call whose handler panicked.
var errHandlerPanicked = errors.New("the device failed to handle the call")

func (s *WampSession) Register(topic topics.Topic, cb func(ctx context.Context, invocation Result) (*InvokeResult, error), options common.Dict) error {
	// Whoever may call the procedure decides how often it fails: every
	// privilege check refuses the callers it does not know. Refusals get a log
	// of their own, so a flood of them hides no genuine failure.
	refused := s.newFailureLog(fmt.Sprintf("refused calls of %s", topic))
	failed := s.newFailureLog(fmt.Sprintf("failed calls of %s", topic))

	invocationHandler := func(ctx context.Context, invocation *wamp.Invocation) (result client.InvokeResult) {
		// The client runs the handler on a goroutine of its own and does not
		// recover it: a handler that panics on a malformed call would take
		// the agent down with it.
		defer func() {
			if r := recover(); r != nil {
				if e := failed.logger().Error(); e.Enabled() {
					e.Msgf("Recovered a panic during invocation of %s: %v\n%s", topic, r, debug.Stack())
				}
				result = client.InvokeResult{
					Err:  wamp.URI("wamp.error.canceled"),
					Args: wamp.List{wamp.Dict{"error": errHandlerPanicked.Error()}},
				}
			}
		}()

		cbInvocationMap := Result{
			Request:      uint64(invocation.Request),
			Registration: uint64(invocation.Registration),
			Details:      common.Dict(invocation.Details),
			Arguments:    invocation.Arguments,
			ArgumentsKw:  common.Dict(invocation.ArgumentsKw),
		}

		resultMap, invokeErr := cb(ctx, cbInvocationMap)
		if invokeErr != nil {
			// A refusal is the check working, not the agent failing: no stack.
			if errdefs.IsInsufficientPrivileges(invokeErr) {
				refused.logger().Warn().Err(invokeErr).Interface("caller_authid", invocation.Details["caller_authid"]).
					Msgf("Refused a call of %s", topic)
			} else {
				failed.logger().Error().Stack().Err(invokeErr).Msgf("An error occured during invocation of %s", topic)
			}
			return client.InvokeResult{
				Err: wamp.URI("wamp.error.canceled"),
				Args: wamp.List{
					wamp.Dict{"error": invokeErr.Error()},
				},
			}
		}

		return client.InvokeResult{Args: resultMap.Arguments, Kwargs: wamp.Dict(resultMap.ArgumentsKw)}
	}

	c := s.currentClient()
	if c == nil {
		return ErrNotConnected
	}

	return c.Register(string(topic), invocationHandler, wamp.Dict{"force_reregister": true})
}

func (s *WampSession) Unregister(topic topics.Topic) error {
	c := s.currentClient()
	if c == nil {
		return ErrNotConnected
	}
	return c.Unregister(string(topic))
}

func (s *WampSession) Unsubscribe(topic topics.Topic) error {
	c := s.currentClient()
	if c == nil {
		return ErrNotConnected
	}
	return c.Unsubscribe(string(topic))
}

// SetupTestament installs the device's WAMP testament on the router so the
// router publishes a DISCONNECTED status if the session drops uncleanly.
func (s *WampSession) SetupTestament() error {
	cfg := s.GetConfig()

	args := []interface{}{
		topics.SetDeviceTestament,
		[]interface{}{
			common.Dict{
				"swarm_key":       cfg.ReswarmConfig.SwarmKey,
				"device_key":      cfg.ReswarmConfig.DeviceKey,
				"serial_number":   cfg.ReswarmConfig.SerialNumber,
				"wamp_session_id": s.GetSessionID(),
			},
		},
		common.Dict{},
	}

	_, err := s.Call(context.Background(), topics.MetaProcAddSessionTestament, args, nil, nil, nil)
	return err
}

// Reconnect drops the current connection so the watcher dials a fresh one.
// The new dial rebuilds the HELLO details from the live config, which is the
// only way to change the session's authid ("<swarm_key>-<device_key>") — it is
// negotiated once per connection. Used after the device is moved to another
// project. No-op when there is no client; the watcher is already reconnecting.
func (s *WampSession) Reconnect() {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()

	if c == nil {
		return
	}

	// Closing makes Done() fire, which is what watchDisconnect is parked on.
	// It re-enters connect(true) and the OnConnect callback runs again.
	_ = c.Close()
}

// Close cancels the session's reconnect loop and closes the underlying
// client. Safe to call multiple times.
func (s *WampSession) Close() {
	s.cancel()

	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()

	if c != nil {
		_ = c.Close()
	}
}

func clientAuthFunc(deviceSecret string) func(c *wamp.Challenge) (string, wamp.Dict) {
	return func(c *wamp.Challenge) (string, wamp.Dict) {
		return crsign.RespondChallenge(deviceSecret, c, nil), wamp.Dict{}
	}
}

func (s *WampSession) UpdateRemoteDeviceStatus(status DeviceStatus) error {
	// Serialized: with probe and send under one slot, payloads reach the
	// backend in probe order, so a stale health reading can never overwrite a
	// fresher one (see statusSlot).
	release, err := s.acquireStatusSlot()
	if err != nil {
		return err
	}
	defer release()

	cfg := s.GetConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// While the device is in a disk-emergency, report EMERGENCY in place of the
	// healthy CONNECTED status so the cloud/UI can flag it (see package diskguard).
	if status == CONNECTED && diskguard.IsEmergency() {
		status = EMERGENCY
	}

	stats := common.GetStats(filepath.Join(cfg.CommandLineArguments.AgentDir, "docker-apps"))

	statsDict := common.Dict{
		"cpu_count":           stats.CPUCount,
		"cpu_usage":           stats.CPUUsagePercent,
		"memory_total":        stats.MemoryTotal,
		"memory_used":         stats.MemoryUsed,
		"memory_available":    stats.MemoryAvailable,
		"storage_total":       stats.StorageTotal,
		"storage_used":        stats.StorageUsed,
		"storage_free":        stats.StorageFree,
		"docker_apps_total":   stats.DockerAppsTotal,
		"docker_apps_used":    stats.DockerAppsUsed,
		"docker_apps_free":    stats.DockerAppsFree,
		"docker_apps_mounted": stats.DockerAppsMounted,
	}

	// Docker daemon health, probed live at send time so the payload is
	// truthful even for outages nothing else has noticed yet (a hung daemon,
	// a stopped Docker Desktop). It rides INSIDE stats deliberately: the
	// backend persists the stats dict verbatim to the device row on every
	// heartbeat and read_device returns it, so the UI badge survives a page
	// reload without a dedicated DB column or migration. The daemon-recovery
	// supervisor's status-transition pushes reuse this exact payload path.
	if s.dockerAvailableFn != nil {
		statsDict["docker_available"] = s.dockerAvailableFn()
	}

	// The file layout (package layout), for following the fleet's move to
	// the current layout (/opt/ironflock/device-agent, the ironflock-device-agent
	// service on Windows); layout_error says why a move was rolled back.
	if cfg.CommandLineArguments.Layout != "" {
		statsDict["layout"] = string(cfg.CommandLineArguments.Layout)
	}
	if layoutErr := migratestate.LastError(); layoutErr != "" {
		statsDict["layout_error"] = layoutErr
	}

	payload := common.Dict{
		"swarm_key":       cfg.ReswarmConfig.SwarmKey,
		"device_key":      cfg.ReswarmConfig.DeviceKey,
		"status":          string(status),
		"wamp_session_id": s.GetSessionID(),
		"stats":           statsDict,
	}

	// Carry per-device tunnel capability on the heartbeat so the UI reflects it
	// live (~30s) without a dedicated get_agent_metadata call. The backend
	// forwards it to the devices store verbatim.
	if s.tunnelCapableFn != nil {
		payload["tunnel_capable"] = s.tunnelCapableFn()
	}

	res, err := s.Call(ctx, topics.UpdateDeviceStatus, []any{payload}, nil, nil, nil)
	if err != nil {
		return err
	}

	// The length, not only the nil-ness: an empty list indexes out of range.
	if len(res.Arguments) == 0 || res.Arguments[0] == nil {
		return nil
	}

	args, ok := res.Arguments[0].(map[string]any)
	if !ok {
		return nil
	}

	// Type-asserted, not fmt.Sprint-ed: an absent key would otherwise cache
	// the literal "<nil>" as the base URL.
	if reswarmBaseURL, ok := args["reswarmBaseURL"].(string); ok && reswarmBaseURL != "" {
		s.agentConfig.ReswarmConfig.ReswarmBaseURL = reswarmBaseURL
	}

	// Appliance backends hand off the effective OTA base on every heartbeat
	// (see applyHandedOffUpdateURL for who takes it and who does not). Persist
	// it so the next start still uses the local mirror; a failed write costs
	// only the persistence, never the heartbeat, and is retried on the next
	// start because the on-disk value then still differs.
	if changed, url := applyHandedOffUpdateURL(s.agentConfig.ReswarmConfig, args["agentUpdateURL"]); changed {
		log.Info().Msgf("agent update base handed off by backend: %s", url)
		cfgPath := s.agentConfig.CommandLineArguments.ConfigFileLocation
		if err := config.SaveReswarmConfig(cfgPath, s.agentConfig.ReswarmConfig); err != nil {
			log.Warn().Err(err).Msgf("failed to persist handed-off agent update base to %s", cfgPath)
		}
	}

	return nil
}

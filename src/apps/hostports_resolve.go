package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"reagent/tunnel"
	"sort"
	"strconv"
	"strings"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"
)

// hostPortKeyForRule builds the (non-compose, unqualified) registry key for a
// tunnel port rule.
func hostPortKeyForRule(stage common.Stage, appKey uint64, rule common.PortForwardRule) hostPortKey {
	return hostPortKey{Stage: stage, AppKey: appKey, Protocol: wireProtocol(rule.Protocol), Port: rule.Port}
}

// reservedHostPortFor returns the rule's user-pinned host port, or 0 when none
// applies. Reservations are PROD tcp/udp only: DEV apps and http/https rules
// carry reserved_* keys inert — ignored without erroring, per the cross-repo
// contract.
func reservedHostPortFor(stage common.Stage, rule common.PortForwardRule) uint64 {
	if !reservationsApply(stage, rule) {
		return 0
	}
	return rule.ReservedHostPort
}

// reservationsApply reports whether reserved_* keys on this rule are honored
// at all (PROD tcp/udp only).
func reservationsApply(stage common.Stage, rule common.PortForwardRule) bool {
	return stage == common.PROD && (rule.Protocol == "tcp" || rule.Protocol == "udp")
}

// reservedPortUnavailableLog is the app-log line for every reservation the
// agent could not honor: the one behavior promise is that it never quietly
// moves the port instead.
func reservedPortUnavailableLog(reservedPort uint64, declaredPort uint64, reason interface{}) string {
	return fmt.Sprintf("Reserved host port %d for port %d is unavailable: %v. Reserved ports are never reassigned automatically - free the port or change the reservation.", reservedPort, declaredPort, reason)
}

// countComposeRuleMatches returns how many of the app's compose port entries a
// rule maps onto. More than one means a reservation is ambiguous: pinning
// would pick a nondeterministic service (reason token: ambiguous_compose_rule).
func countComposeRuleMatches(dockerCompose map[string]interface{}, rule common.PortForwardRule) int {
	matches := 0
	for _, entry := range parseComposePorts(dockerCompose) {
		if entry.matchesRule(rule.Port, rule.Protocol) {
			matches++
		}
	}
	return matches
}

// CheckHostPortAvailability answers the check_host_port RPC: whether the rule
// identified by (appKey, stage, protocol, declaredPort) could take hostPort as
// its reserved host port right now. Reason tokens per the cross-repo contract:
// "taken_by_app", "taken_by_process", "ambiguous_compose_rule", "denylisted",
// "" when available.
func (am *AppManager) CheckHostPortAvailability(appKey uint64, stage common.Stage, protocol string, declaredPort uint64, hostPort uint64) (bool, string) {
	rule := common.PortForwardRule{Port: declaredPort, Protocol: protocol}
	key := hostPortKeyForRule(stage, appKey, rule)

	// Qualify compose keys by service so the check matches the pin the launch
	// path would attempt; an app not (yet) known locally checks unqualified.
	if statePayload, err := am.AppStore.GetRequestedState(appKey, stage); err == nil && statePayload.DockerCompose != nil {
		if countComposeRuleMatches(statePayload.DockerCompose, rule) > 1 {
			return false, "ambiguous_compose_rule"
		}
		for _, entry := range parseComposePorts(statePayload.DockerCompose) {
			if entry.matchesRule(rule.Port, rule.Protocol) {
				key.Service = entry.Service
				break
			}
		}
	}

	return am.hostPorts.CheckAvailability(key, hostPort)
}

// publishedComposeHostPorts reads the host ports the app's compose project
// currently publishes, keyed like container.PublishedPortKey. Recovering a
// still-running previous generation's ports keeps them stable across restarts
// and agent updates. It reads them from the Docker API rather than
// `docker compose ps`: this runs on the transition's critical section, and on
// boot — exactly when a restarted agent recovers ports — dockerd is at its
// most contended, where a compose-CLI invocation can stall the whole
// transition for tens of seconds. An empty map when the project is not up (its
// ports then come from the persisted host_port hints instead).
func (am *AppManager) publishedComposeHostPorts(payload common.TransitionPayload) map[string]uint64 {
	project := common.BuildComposeContainerName(payload.Stage, payload.AppKey, payload.AppName)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	published, err := am.StateMachine.Container.GetComposePublishedPorts(ctx, project)
	if err != nil {
		log.Debug().Err(err).Str("app", payload.AppName).Msg("Could not read published compose ports")
		return map[string]uint64{}
	}
	return published
}

// bindingKey is how GetContainerPortBindings keys a container port.
func bindingKey(port uint64, protocol string) string {
	return fmt.Sprintf("%d/%s", port, protocol)
}

// resolveTunnelHostPort returns the local port frpc should dial for a port
// rule. It never allocates fresh pool ports — that happens at container
// launch — it only reads the registry or recovers assignments from the app's
// live docker state and the host_port persisted upstream.
//
// ok=false means the app has not been started with managed ports yet; the
// caller skips tunnel creation and the post-transition sync retries. A
// non-empty reservationErr means ok=false is a reservation the agent could
// not honor (the caller surfaces it on the rule instead of silently waiting).
func (am *AppManager) resolveTunnelHostPort(payload common.TransitionPayload, rule common.PortForwardRule) (hostPort uint64, ok bool, reservationErr string) {
	stage, appKey := payload.Stage, payload.AppKey
	protocol := wireProtocol(rule.Protocol)

	if reservedPort := reservedHostPortFor(stage, rule); reservedPort != 0 {
		key := hostPortKeyForRule(stage, appKey, rule)
		livePort := uint64(0)
		if payload.DockerCompose != nil {
			if countComposeRuleMatches(payload.DockerCompose, rule) > 1 {
				log.Warn().Str("app", payload.AppName).Uint64("port", rule.Port).Msg("Reserved host port is ambiguous across compose services; deferring tunnel")
				return 0, false, fmt.Sprintf("reserved host port %d is ambiguous: several compose services declare port %d (ambiguous_compose_rule)", reservedPort, rule.Port)
			}
			if service, published := am.findComposeRulePort(payload, rule); service != "" {
				key.Service = service
				livePort = published
			}
		} else {
			// Reserved rules exist only on PROD (see reservationsApply), so
			// the PROD container name is the right one to inspect.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
			defer cancel()
			if bindings, err := am.StateMachine.Container.GetContainerPortBindings(ctx, payload.ContainerName.Prod); err == nil {
				livePort = bindings[bindingKey(rule.Port, protocol)]
			}
		}

		// Pin regardless of what the container currently publishes: the pool
		// must never hand the reserved port to another app while a not-yet-
		// applied reservation waits for its container recreate. skipProbe: the
		// reservation was validated at launch; the bind the probe would trip
		// over is our own published container's.
		if err := am.hostPorts.PinExact(key, reservedPort, true); err != nil {
			// Never dial a wrong port for a reserved rule: defer the tunnel.
			log.Warn().Err(err).Str("app", payload.AppName).Uint64("port", rule.Port).Uint64("reservedHostPort", reservedPort).Msg("Reserved host port is unavailable; deferring tunnel")
			return 0, false, err.Error()
		}

		if livePort != 0 && livePort != reservedPort {
			// The running container still publishes its pre-reservation port:
			// a reservation saved on a RUNNING app arrives via a sync that
			// never recreates the container, so nothing binds the reserved
			// port yet. Dialing it would break the working tunnel and write
			// back a host_port nothing serves. Keep the tunnel on the port
			// that actually answers — the host_port write-back stays truthful
			// (the UI shows the reservation as pending) and the reservation
			// takes effect at the next container recreate via the launch path
			// (the pin above keeps the port from the pool meanwhile).
			am.notePendingReservation(key, reservedPort, payload.ContainerName.Prod, rule.Port, livePort)
			return livePort, true, ""
		}

		am.clearPendingReservationNote(key)
		return reservedPort, true, ""
	}

	// A pin left behind by a removed reservation must not keep steering the
	// dial port (or blocking reassignment): drop it, so the host port is
	// re-derived from the live container state below. Matters most for a
	// reservation removed while still pending — the pinned port was never
	// bound by the container, and recovering it here would break the working
	// tunnel exactly like dialing an unapplied reservation would.
	if am.hostPorts.ReleaseStalePin(stage, appKey, protocol, rule.Port) {
		am.clearPendingReservationNote(hostPortKeyForRule(stage, appKey, rule))
	}

	if hostPort, ok := am.hostPorts.GetByPort(stage, appKey, protocol, rule.Port); ok {
		return hostPort, true, ""
	}

	key := hostPortKeyForRule(stage, appKey, rule)
	preferred := rule.HostPort

	if payload.DockerCompose != nil {
		service, published := am.findComposeRulePort(payload, rule)
		if service != "" {
			// Use the service-qualified key so the recovery converges with
			// the assignment SetupComposeFiles makes for the same entry.
			key.Service = service
		}
		if published != 0 {
			preferred = published
		}
	} else {
		containerName := payload.ContainerName.Prod
		if stage == common.DEV {
			containerName = payload.ContainerName.Dev
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		bindings, err := am.StateMachine.Container.GetContainerPortBindings(ctx, containerName)
		if err == nil {
			if bound := bindings[bindingKey(rule.Port, protocol)]; bound != 0 {
				preferred = bound
			} else if len(bindings) == 0 {
				// A pre-migration container still running with host
				// networking: the declared port IS the host port. Dial it
				// directly without recording a managed assignment — the
				// container is recreated onto a bridge on its next start.
				networkMode, err := am.StateMachine.Container.GetContainerNetworkMode(ctx, containerName)
				if err == nil && networkMode == "host" {
					return rule.Port, true, ""
				}
			}
		}
	}

	if preferred == 0 {
		return 0, false, ""
	}

	recovered, err := am.hostPorts.RecoverOrReserve(key, preferred)
	if err != nil {
		log.Error().Err(err).Str("app", payload.AppName).Uint64("port", rule.Port).Msg("Failed to recover host port for tunnel")
		return 0, false, ""
	}
	return recovered, true, ""
}

// findComposeRulePort locates the compose entry a port rule refers to and
// returns its service name plus the host port the running project currently
// publishes it on (0 when the project is not up).
func (am *AppManager) findComposeRulePort(payload common.TransitionPayload, rule common.PortForwardRule) (string, uint64) {
	for _, entry := range parseComposePorts(payload.DockerCompose) {
		if !entry.matchesRule(rule.Port, rule.Protocol) {
			continue
		}

		published := am.publishedComposeHostPorts(payload)
		return entry.Service, published[container.PublishedPortKey(entry.Service, entry.ContainerPort, entry.Protocol)]
	}
	return "", 0
}

// reserveLaunchHostPort picks the host port to publish a single-container
// app's port rule on at (re)creation time, allocating from the pool when
// nothing can be recovered. DEV apps get their declared port when no other
// app holds it, so developers keep reaching dev apps on the port they wrote;
// the caller surfaces the fallback port in the app log when that loses.
func (am *AppManager) reserveLaunchHostPort(payload common.TransitionPayload, rule common.PortForwardRule, liveBindings map[string]uint64) (uint64, error) {
	key := hostPortKeyForRule(payload.Stage, payload.AppKey, rule)

	if reservedPort := reservedHostPortFor(payload.Stage, rule); reservedPort != 0 {
		// Exact-or-fail: a pin failure fails the whole transition rather than
		// launching on a different port. Probe skipped only when our own live
		// binding already publishes exactly the reserved port.
		skipProbe := liveBindings[bindingKey(rule.Port, key.Protocol)] == reservedPort
		if err := am.hostPorts.PinExact(key, reservedPort, skipProbe); err != nil {
			return 0, err
		}
		return reservedPort, nil
	}

	preferred := liveBindings[bindingKey(rule.Port, key.Protocol)]
	if preferred == 0 {
		preferred = rule.HostPort
	}

	if payload.Stage == common.DEV && preferred == 0 && am.hostPorts.ReserveDeclared(key) {
		return key.Port, nil
	}

	return am.hostPorts.RecoverOrReserve(key, preferred)
}

// computePortBindings builds the ExposedPorts/PortBindings that publish each
// of the app's declared ports on its agent-managed host port, bound on all
// interfaces so devices stay reachable on the LAN.
func (sm *StateMachine) computePortBindings(payload common.TransitionPayload, portRules []common.PortForwardRule, containerName string) (nat.PortSet, nat.PortMap, error) {
	am := sm.StateObserver.AppManager

	publishableRules := make([]common.PortForwardRule, 0, len(portRules))
	for _, rule := range portRules {
		// Rules with an explicit LocalIP point away from this container;
		// nothing to publish.
		if rule.LocalIP == "" {
			publishableRules = append(publishableRules, rule)
		}
	}

	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}
	if len(publishableRules) == 0 {
		return exposedPorts, portBindings, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	liveBindings, err := sm.Container.GetContainerPortBindings(ctx, containerName)
	if err != nil {
		if !errdefs.IsContainerNotFound(err) {
			log.Debug().Err(err).Str("container", containerName).Msg("Could not inspect existing container for host port recovery")
		}
		liveBindings = map[string]uint64{}
	}

	for _, rule := range publishableRules {
		hostPort, err := am.reserveLaunchHostPort(payload, rule, liveBindings)
		if err != nil {
			if reservedPort := reservedHostPortFor(payload.Stage, rule); reservedPort != 0 {
				_ = sm.LogManager.Write(containerName, reservedPortUnavailableLog(reservedPort, rule.Port, err))
			}
			return nil, nil, err
		}

		if payload.Stage == common.DEV && hostPort != rule.Port {
			_ = sm.LogManager.Write(containerName, fmt.Sprintf("Port %d is already in use by another app; port %d of %s is reachable on host port %d instead.", rule.Port, rule.Port, payload.AppName, hostPort))
		}

		containerPort, err := nat.NewPort(wireProtocol(rule.Protocol), strconv.FormatUint(rule.Port, 10))
		if err != nil {
			return nil, nil, err
		}

		exposedPorts[containerPort] = struct{}{}
		portBindings[containerPort] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.FormatUint(hostPort, 10)}}
	}

	return exposedPorts, portBindings, nil
}

// devicePortEnvLines renders one DEVICE_PORT_FOR_<declared port>=<host port>
// line per entry, sorted by declared port.
func devicePortEnvLines(hostPortByDeclared map[uint64]uint64) []string {
	declaredPorts := make([]uint64, 0, len(hostPortByDeclared))
	for declared := range hostPortByDeclared {
		declaredPorts = append(declaredPorts, declared)
	}
	sort.Slice(declaredPorts, func(i, j int) bool { return declaredPorts[i] < declaredPorts[j] })

	lines := make([]string, 0, len(declaredPorts))
	for _, declared := range declaredPorts {
		lines = append(lines, fmt.Sprintf("DEVICE_PORT_FOR_%d=%d", declared, hostPortByDeclared[declared]))
	}
	return lines
}

// devicePortEnvsFromBindings builds the DEVICE_PORT_FOR_<port> environment
// variables announcing which host port each published port landed on.
// Single-container apps publish declared ports verbatim, so the container
// port doubles as the declared port. When tcp and udp share a declared port
// the tcp binding wins (deterministic via the sorted "port/proto" keys).
func devicePortEnvsFromBindings(portBindings nat.PortMap) []string {
	containerPorts := make([]nat.Port, 0, len(portBindings))
	for containerPort := range portBindings {
		containerPorts = append(containerPorts, containerPort)
	}
	sort.Slice(containerPorts, func(i, j int) bool { return containerPorts[i] < containerPorts[j] })

	hostPortByDeclared := make(map[uint64]uint64, len(containerPorts))
	for _, containerPort := range containerPorts {
		declared := uint64(containerPort.Int())
		if _, ok := hostPortByDeclared[declared]; ok {
			continue
		}
		for _, hostBinding := range portBindings[containerPort] {
			hostPort, err := strconv.ParseUint(hostBinding.HostPort, 10, 64)
			if err == nil && hostPort != 0 {
				hostPortByDeclared[declared] = hostPort
				break
			}
		}
	}

	return devicePortEnvLines(hostPortByDeclared)
}

// devicePortEnvsForCompose builds the DEVICE_PORT_FOR_<port> environment
// variables for a compose app by reading the registry assignments
// rewriteComposeHostPorts recorded for it. dockerCompose must be the authored
// definition — the rewrite replaces the authored host ports, which are the
// entries' declared identity. Unmanaged entries (ranges, variables) and
// container-only ports no rule publishes carry no assignment and are skipped.
// When two services share a declared port the first (sorted by service) wins.
func (am *AppManager) devicePortEnvsForCompose(payload common.TransitionPayload, dockerCompose map[string]interface{}) []string {
	entries := parseComposePorts(dockerCompose)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DeclaredPort() != entries[j].DeclaredPort() {
			return entries[i].DeclaredPort() < entries[j].DeclaredPort()
		}
		if entries[i].Service != entries[j].Service {
			return entries[i].Service < entries[j].Service
		}
		return entries[i].Protocol < entries[j].Protocol
	})

	hostPortByDeclared := make(map[uint64]uint64)
	for _, entry := range entries {
		if !entry.Rewritable {
			continue
		}
		if _, ok := hostPortByDeclared[entry.DeclaredPort()]; ok {
			continue
		}

		key := hostPortKey{Stage: payload.Stage, AppKey: payload.AppKey, Protocol: entry.Protocol, Port: entry.DeclaredPort(), Service: entry.Service}
		if hostPort, ok := am.hostPorts.Get(key); ok {
			hostPortByDeclared[entry.DeclaredPort()] = hostPort
		}
	}

	return devicePortEnvLines(hostPortByDeclared)
}

// rewriteComposeHostPorts replaces the host side of every published compose
// port with an agent-managed host port bound on all interfaces, so compose
// apps cannot collide on host ports. Container-only entries are published
// only when a port rule references them; entries the parser cannot handle
// (ranges, compose variables) are left byte-for-byte as authored.
//
// The map passed in must be a copy of the authored compose definition — the
// rewrite mutates it.
func (sm *StateMachine) rewriteComposeHostPorts(payload common.TransitionPayload, dockerCompose map[string]interface{}) error {
	am := sm.StateObserver.AppManager

	published := am.publishedComposeHostPorts(payload)

	portRules, err := tunnel.InterfaceToPortForwardRule(payload.Ports)
	if err != nil {
		return err
	}

	// A reservation must map onto exactly one compose entry: when several
	// services declare the same port, pinning would land on a nondeterministic
	// service, so the launch fails instead (reason: ambiguous_compose_rule).
	for _, rule := range portRules {
		if reservedPort := reservedHostPortFor(payload.Stage, rule); reservedPort != 0 && countComposeRuleMatches(dockerCompose, rule) > 1 {
			ambiguityErr := fmt.Errorf("reserved host port %d for port %d is ambiguous: several compose services declare the port (ambiguous_compose_rule)", reservedPort, rule.Port)
			_ = sm.LogManager.Write(payload.ContainerName.Prod, reservedPortUnavailableLog(reservedPort, rule.Port, "several compose services declare the same port"))
			return ambiguityErr
		}
	}

	services, ok := dockerCompose["services"].(map[string]interface{})
	if !ok {
		return nil
	}

	for serviceName, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}

		ports, ok := service["ports"].([]interface{})
		if !ok {
			continue
		}

		rewritten := make([]interface{}, 0, len(ports))
		for _, raw := range ports {
			entry := parseComposePortEntry(raw)
			entry.Service = serviceName

			if !entry.Rewritable {
				log.Warn().Str("app", payload.AppName).Str("service", serviceName).Interface("entry", raw).Msg("Compose port entry left unmanaged (range, variable or unparseable); it may collide with other apps")
				rewritten = append(rewritten, raw)
				continue
			}

			var matchingRule *common.PortForwardRule
			for i, rule := range portRules {
				if rule.LocalIP == "" && entry.matchesRule(rule.Port, rule.Protocol) {
					matchingRule = &portRules[i]
					break
				}
			}

			if entry.HostPort == 0 && matchingRule == nil {
				// Container-only port no rule asks for: keep it unpublished.
				rewritten = append(rewritten, raw)
				continue
			}

			preferred := published[container.PublishedPortKey(serviceName, entry.ContainerPort, entry.Protocol)]
			if preferred == 0 && matchingRule != nil {
				preferred = matchingRule.HostPort
			}

			key := hostPortKey{Stage: payload.Stage, AppKey: payload.AppKey, Protocol: entry.Protocol, Port: entry.DeclaredPort(), Service: serviceName}

			var hostPort uint64
			if matchingRule != nil && reservedHostPortFor(payload.Stage, *matchingRule) != 0 {
				reservedPort := matchingRule.ReservedHostPort
				// Exact-or-fail; probe skipped only when the running project
				// already publishes the entry on exactly the reserved port.
				skipProbe := published[container.PublishedPortKey(serviceName, entry.ContainerPort, entry.Protocol)] == reservedPort
				if pinErr := am.hostPorts.PinExact(key, reservedPort, skipProbe); pinErr != nil {
					_ = sm.LogManager.Write(payload.ContainerName.Prod, reservedPortUnavailableLog(reservedPort, matchingRule.Port, pinErr))
					return pinErr
				}
				hostPort = reservedPort
			} else if payload.Stage == common.DEV && preferred == 0 && am.hostPorts.ReserveDeclared(key) {
				// DEV keeps the authored host port so developers reach the
				// app where they expect it.
				hostPort = key.Port
			} else {
				hostPort, err = am.hostPorts.RecoverOrReserve(key, preferred)
				if err != nil {
					return err
				}

				if payload.Stage == common.DEV && hostPort != entry.DeclaredPort() {
					_ = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Port %d is already in use by another app; port %d of service %s is reachable on host port %d instead.", entry.DeclaredPort(), entry.ContainerPort, serviceName, hostPort))
				}
			}

			rewritten = append(rewritten, rewriteComposePortEntry(entry, "0.0.0.0", hostPort))
		}

		service["ports"] = rewritten
	}

	return nil
}

// containerNetworkConfigOutdated reports whether an existing container must
// be recreated because its network mode or published ports no longer match
// the desired host config.
func (sm *StateMachine) containerNetworkConfigOutdated(cont dockercontainer.Summary, hConfig *dockercontainer.HostConfig, containerName string) bool {
	if normalizeNetworkMode(cont.HostConfig.NetworkMode) != normalizeNetworkMode(string(hConfig.NetworkMode)) {
		return true
	}

	if len(hConfig.PortBindings) == 0 {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	actualBindings, err := sm.Container.GetContainerPortBindings(ctx, containerName)
	if err != nil {
		return false
	}

	desiredBindings := make(map[string]uint64, len(hConfig.PortBindings))
	for containerPort, hostBindings := range hConfig.PortBindings {
		for _, hostBinding := range hostBindings {
			hostPort, err := strconv.ParseUint(hostBinding.HostPort, 10, 64)
			if err == nil {
				desiredBindings[string(containerPort)] = hostPort
			}
		}
	}

	if len(desiredBindings) != len(actualBindings) {
		return true
	}
	for containerPort, hostPort := range desiredBindings {
		if actualBindings[containerPort] != hostPort {
			return true
		}
	}

	return false
}

func normalizeNetworkMode(mode string) string {
	if mode == "" || mode == "default" {
		return "bridge"
	}
	return mode
}

// writeAppLog writes one line to the app's log stream, tolerating the partial
// managers unit tests construct (no StateMachine/LogManager wired).
func (am *AppManager) writeAppLog(containerName string, message string) {
	if am.StateMachine == nil || am.StateMachine.LogManager == nil {
		return
	}
	_ = am.StateMachine.LogManager.Write(containerName, message)
}

// notePendingReservation writes the "takes effect on the next restart"
// app-log line for a reservation applied to a running container, once per
// (rule, reserved port): syncPortState runs on every sync pass and the
// pending state persists until the container is next recreated, so an unpaced
// line would spam the user-visible app log forever.
func (am *AppManager) notePendingReservation(key hostPortKey, reservedPort uint64, containerName string, declaredPort uint64, livePort uint64) {
	am.reservationNoteLock.Lock()
	if am.reservationNotes == nil {
		am.reservationNotes = make(map[hostPortKey]uint64)
	}
	noted := am.reservationNotes[key] == reservedPort
	if !noted {
		am.reservationNotes[key] = reservedPort
	}
	am.reservationNoteLock.Unlock()

	if noted {
		return
	}
	am.writeAppLog(containerName, fmt.Sprintf("Reserved host port %d for port %d takes effect on the next restart of the app; until then port %d stays reachable on host port %d.", reservedPort, declaredPort, declaredPort, livePort))
}

// clearPendingReservationNote forgets the pacing state once the reservation
// is live (or removed), so a future pending reservation on the same rule is
// announced again. Matches across compose service qualifiers, like the pins.
func (am *AppManager) clearPendingReservationNote(key hostPortKey) {
	am.reservationNoteLock.Lock()
	for noted := range am.reservationNotes {
		if sameRule(noted, key) {
			delete(am.reservationNotes, noted)
		}
	}
	am.reservationNoteLock.Unlock()
}

// reassignComposePortsAfterBindConflict is reassignPortsAfterBindConflict for
// compose apps. It walks the compose `ports:` entries rather than the tunnel
// port rules: compose assignments are recorded under service-qualified keys
// (rewriteComposeHostPorts), which a rule-derived key never matches, and a
// compose app can publish ports no tunnel rule mentions at all.
func (am *AppManager) reassignComposePortsAfterBindConflict(payload common.TransitionPayload, bindErr error) bool {
	if payload.DockerCompose == nil {
		return false
	}

	message := bindErr.Error()
	reassigned := false
	for _, entry := range parseComposePorts(payload.DockerCompose) {
		if !entry.Rewritable {
			continue
		}

		key := hostPortKey{
			Stage:    payload.Stage,
			AppKey:   payload.AppKey,
			Protocol: entry.Protocol,
			Port:     entry.DeclaredPort(),
			Service:  entry.Service,
		}

		hostPort, ok := am.hostPorts.Get(key)
		if !ok || !strings.Contains(message, fmt.Sprintf(":%d", hostPort)) {
			continue
		}

		if am.hostPorts.IsPinned(key) {
			// A reserved port is never reassigned: when only pinned ports
			// conflicted this returns false and the bind error becomes a
			// FAILED transition.
			am.writeAppLog(payload.ContainerName.Prod, reservedPortUnavailableLog(hostPort, entry.DeclaredPort(), "the port is in use by another process"))
			continue
		}

		newPort, err := am.hostPorts.ReassignFresh(key)
		if err != nil {
			log.Error().Err(err).Str("service", entry.Service).Uint64("port", entry.DeclaredPort()).Msg("Failed to reassign compose host port after bind conflict")
			continue
		}

		log.Warn().Str("app", payload.AppName).Str("service", entry.Service).Uint64("port", entry.DeclaredPort()).Uint64("oldHostPort", hostPort).Uint64("newHostPort", newPort).Msg("Host port was taken by another process, reassigned")
		reassigned = true
	}

	return reassigned
}

// isPortAllocationError reports whether a container create/start failure is a
// host port conflict (something outside the registry grabbed the port between
// probe and bind). For compose apps the conflict is reported by the CLI on
// stderr, which reaches here inside a container.ComposeError.
func isPortAllocationError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "port is already allocated") || strings.Contains(message, "address already in use")
}

// reassignPortsAfterBindConflict drops the app's registry assignments that
// appear in a bind-conflict error and allocates fresh ports for them. Returns
// true when at least one port was reassigned, i.e. a retry makes sense.
func (am *AppManager) reassignPortsAfterBindConflict(payload common.TransitionPayload, bindErr error) bool {
	portRules, err := tunnel.InterfaceToPortForwardRule(payload.Ports)
	if err != nil {
		return false
	}

	message := bindErr.Error()
	reassigned := false
	for _, rule := range portRules {
		if rule.LocalIP != "" {
			continue
		}

		key := hostPortKeyForRule(payload.Stage, payload.AppKey, rule)
		hostPort, ok := am.hostPorts.Get(key)
		if !ok || !strings.Contains(message, fmt.Sprintf(":%d", hostPort)) {
			continue
		}

		if am.hostPorts.IsPinned(key) {
			// A reserved port is never reassigned: when only pinned ports
			// conflicted this returns false and the bind error becomes a
			// FAILED transition.
			am.writeAppLog(payload.ContainerName.Prod, reservedPortUnavailableLog(hostPort, rule.Port, "the port is in use by another process"))
			continue
		}

		newPort, err := am.hostPorts.ReassignFresh(key)
		if err != nil {
			log.Error().Err(err).Uint64("port", rule.Port).Msg("Failed to reassign host port after bind conflict")
			continue
		}

		log.Warn().Str("app", payload.AppName).Uint64("port", rule.Port).Uint64("oldHostPort", hostPort).Uint64("newHostPort", newPort).Msg("Host port was taken by another process, reassigned")
		reassigned = true
	}

	return reassigned
}

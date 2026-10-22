package apps

import (
	"fmt"
	"net"
	"reagent/common"
	"sync"
)

// App container host ports are allocated from this range. It sits above the
// frpc admin webserver ports (tunnel's pickAdminPort scans up from 7411) and
// the appliance tunnel data-plane range (30000-30049).
const (
	hostPortRangeStart uint64 = 40000
	hostPortRangeEnd   uint64 = 49999
)

// Agent-internal listeners a user-pinned host port must never take. Pinned
// ports may live outside the 40000-49999 pool, so they can collide with
// infrastructure the pool was deliberately placed above.
const (
	// frpc's loopback admin webserver band: pickAdminPort (tunnel/frp.go,
	// adminPortScanStart) scans up from 7411; 7400 is the local-dev frps port
	// skipped by that scan, and frps' own control port is 7000.
	frpcAdminPortBandStart uint64 = 7000
	frpcAdminPortBandEnd   uint64 = 7499
)

// pinnedHostPortDenylisted reports whether a user-pinned host port would
// shadow an agent-internal listener. Reason token for the availability
// contract: "denylisted". Only ports the agent may listen on at ANY moment
// belong here — the agent's pprof endpoint (port 80 by default) exists only
// when -profiling is passed, so a live one is caught by the OS probe
// (taken_by_process) instead of statically banning port 80 for web services.
func pinnedHostPortDenylisted(port uint64) bool {
	return port >= frpcAdminPortBandStart && port <= frpcAdminPortBandEnd
}

// hostPortKey identifies one published app port by its declared (app-facing)
// port, not by the host port it happens to be mapped to. Service qualifies
// compose services ("" for single-container apps) so two services of the same
// app may expose the same container port.
type hostPortKey struct {
	Stage    common.Stage
	AppKey   uint64
	Protocol string
	Port     uint64
	Service  string
}

// sameRule reports whether two keys refer to the same port rule, ignoring the
// compose Service qualifier: rules carry no service name, so identities
// derived from a rule and from a compose entry differ only there.
func sameRule(a hostPortKey, b hostPortKey) bool {
	return a.Stage == b.Stage && a.AppKey == b.AppKey && a.Protocol == b.Protocol && a.Port == b.Port
}

// portClaim is how the registry tracks an occupied host port: tcp and udp are
// separate namespaces on the host, so a udp hold must never block (nor its
// release free) the same numeric tcp port.
type portClaim struct {
	Protocol string
	Port     uint64
}

// reservedHostPort is one user-pinned host port as reported by the
// reservations provider: the pinned port and the identity of the rule that
// owns it (Service always "", since rules carry no service name).
type reservedHostPort struct {
	Protocol string
	Port     uint64
	Owner    hostPortKey
}

// HostPortRegistry is the agent's single source of truth for which host ports
// it has handed out to app containers. It is in-memory only: after an agent
// restart it is repopulated lazily from the actual container port bindings
// and from the host_port values persisted in t_device_to_app.ports, both of
// which re-enter through RecoverOrReserve's preferred argument.
type HostPortRegistry struct {
	mu       sync.Mutex
	assigned map[hostPortKey]uint64
	inUse    map[portClaim]struct{}
	// pinned marks keys whose assignment is a user reservation: it must never
	// be moved by the agent (ReassignFresh refuses it, the bind-conflict
	// retries skip it). Cleared with the assignment on release.
	pinned map[hostPortKey]struct{}
	// reservations returns every user-pinned host port across ALL installed
	// apps (running or not), so the pool scan and availability checks honor
	// reservations that have no registry assignment yet. Consulted live on
	// every decision rather than seeded once at boot. Nil means none known.
	// Must not call back into the registry (invoked under mu).
	reservations func() []reservedHostPort
	// probeFree reports whether the OS would let us bind the port right now.
	// Swappable for tests.
	probeFree func(protocol string, port uint64) bool
}

func NewHostPortRegistry() *HostPortRegistry {
	return &HostPortRegistry{
		assigned:  make(map[hostPortKey]uint64),
		inUse:     make(map[portClaim]struct{}),
		pinned:    make(map[hostPortKey]struct{}),
		probeFree: probeHostPortFree,
	}
}

// SetReservationsProvider installs the hook the registry consults for
// device-wide reserved host ports (set by AppManager).
func (r *HostPortRegistry) SetReservationsProvider(provider func() []reservedHostPort) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reservations = provider
}

// probeHostPortFree checks bindability on all interfaces, matching how app
// ports are published (0.0.0.0). UDP rules probe a UDP socket; everything
// else (tcp/http/https) rides on TCP.
func probeHostPortFree(protocol string, port uint64) bool {
	addr := fmt.Sprintf(":%d", port)
	if protocol == "udp" {
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (r *HostPortRegistry) Get(key hostPortKey) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	port, ok := r.assigned[key]
	return port, ok
}

// IsPinned reports whether key's assignment is a user reservation.
func (r *HostPortRegistry) IsPinned(key hostPortKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, ok := r.pinned[key]
	return ok
}

// GetByPort returns the assignment matching stage/appKey/protocol/port for
// any compose service. Tunnel port rules carry no service name, so this is
// how the tunnel path finds the host port that compose file generation
// allocated. When several services of one app expose the same declared port
// the result is ambiguous — a pre-existing limitation, since the tunnel
// subdomain is derived from the declared port alone.
func (r *HostPortRegistry) GetByPort(stage common.Stage, appKey uint64, protocol string, port uint64) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for key, hostPort := range r.assigned {
		if key.Stage == stage && key.AppKey == appKey && key.Protocol == protocol && key.Port == port {
			return hostPort, true
		}
	}
	return 0, false
}

// RecoverOrReserve returns the host port for key, assigning one if needed.
//
// A non-zero preferred port is claimed verbatim (unless another app holds
// it): it was recovered from a live container binding or a persisted
// host_port, so the OS-level bind that would make a probe fail may be our own
// container's. A stale preferred that some foreign process occupies surfaces
// as a bind error at container start, which the caller handles by retrying
// via ReassignFresh.
func (r *HostPortRegistry) RecoverOrReserve(key hostPortKey, preferred uint64) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if port, ok := r.assigned[key]; ok {
		return port, nil
	}

	if preferred > 0 {
		if _, taken := r.inUse[portClaim{key.Protocol, preferred}]; !taken && !r.reservedByOtherLocked(key, preferred) {
			r.record(key, preferred)
			return preferred, nil
		}
	}

	return r.reserveFreeLocked(key)
}

// PinExact claims exactly port for key — a user reservation: exact-or-fail,
// never a pool fallback (RecoverOrReserve's preferred-taken fallback silently
// pool-allocates, which for a reservation is the one forbidden behavior).
// Allowed outside the 40000-49999 pool. skipProbe is for recovery: when our
// own live binding already publishes exactly this port the failing OS bind is
// ours, same rationale as RecoverOrReserve's preferred handling.
func (r *HostPortRegistry) PinExact(key hostPortKey, port uint64, skipProbe bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if pinnedHostPortDenylisted(port) {
		return fmt.Errorf("reserved host port %d is denylisted (agent-internal port)", port)
	}

	if current, ok := r.assigned[key]; ok {
		if current == port {
			r.pinned[key] = struct{}{}
			return nil
		}
		// The key held a pool-allocated (or previously reserved) port; the
		// reservation replaces it. Free the old claim before taking the new.
		delete(r.assigned, key)
		delete(r.inUse, portClaim{key.Protocol, current})
		delete(r.pinned, key)
	}

	if _, taken := r.inUse[portClaim{key.Protocol, port}]; taken {
		return fmt.Errorf("reserved host port %d is already used by another app", port)
	}

	if !skipProbe && !r.probeFree(key.Protocol, port) {
		return fmt.Errorf("reserved host port %d is in use by another process", port)
	}

	r.record(key, port)
	r.pinned[key] = struct{}{}
	return nil
}

// CheckAvailability reports whether key could pin port right now, with the
// reason token of the check_host_port contract when it cannot: "denylisted",
// "taken_by_app", "taken_by_process", "" when available. The rule's own
// current assignment/reservation counts as available (re-submitting the port
// a rule already holds must not fail against our own bind).
func (r *HostPortRegistry) CheckAvailability(key hostPortKey, port uint64) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if pinnedHostPortDenylisted(port) {
		return false, "denylisted"
	}

	ours := false
	if current, ok := r.assigned[key]; ok && current == port {
		ours = true
	}

	if r.reservations != nil {
		for _, reservation := range r.reservations() {
			if reservation.Protocol != key.Protocol || reservation.Port != port {
				continue
			}
			if sameRule(reservation.Owner, key) {
				ours = true
				continue
			}
			return false, "taken_by_app"
		}
	}

	if !ours {
		for holder, hostPort := range r.assigned {
			if holder.Protocol == key.Protocol && hostPort == port && !sameRule(holder, key) {
				return false, "taken_by_app"
			}
		}
	}

	if ours {
		return true, ""
	}

	if !r.probeFree(key.Protocol, port) {
		return false, "taken_by_process"
	}

	return true, ""
}

// ReleaseStalePin drops a pinned assignment recorded for the rule (matched
// across compose service qualifiers, since tunnel rules carry none) — used
// when the rule no longer carries a host-port reservation. Without this, a
// reservation applied while the app was running (pinned but pending, the
// container never recreated) and then removed would keep steering the tunnel
// dial and the host_port write-back onto a port nothing binds, and would keep
// refusing ReassignFresh. Returns true when a pin was dropped; unpinned pool
// assignments are left alone.
func (r *HostPortRegistry) ReleaseStalePin(stage common.Stage, appKey uint64, protocol string, port uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	ruleKey := hostPortKey{Stage: stage, AppKey: appKey, Protocol: protocol, Port: port}
	for key := range r.pinned {
		if !sameRule(key, ruleKey) {
			continue
		}
		if assigned, ok := r.assigned[key]; ok {
			delete(r.assigned, key)
			delete(r.inUse, portClaim{key.Protocol, assigned})
		}
		delete(r.pinned, key)
		return true
	}
	return false
}

// ReserveDeclared claims key.Port itself as the host port (the DEV rule:
// developers reach dev apps on the declared port). It only guards against
// ports held by other agent-managed apps — a foreign process squatting the
// port surfaces as a Docker bind error, exactly as it did before this
// registry existed. Returns false when another app already holds the port,
// in which case the caller falls back to RecoverOrReserve.
func (r *HostPortRegistry) ReserveDeclared(key hostPortKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if port, ok := r.assigned[key]; ok && port == key.Port {
		return true
	}

	if _, taken := r.inUse[portClaim{key.Protocol, key.Port}]; taken {
		return false
	}
	if r.reservedByOtherLocked(key, key.Port) {
		return false
	}

	r.record(key, key.Port)
	return true
}

// ReassignFresh drops key's current assignment and allocates a new port from
// the pool. Used when binding the assigned port failed because something
// outside the agent grabbed it. Pinned keys are refused: a user reservation
// is never reassigned, the conflict must surface as a failure instead.
func (r *HostPortRegistry) ReassignFresh(key hostPortKey) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, isPinned := r.pinned[key]; isPinned {
		return 0, fmt.Errorf("host port of (%s, %d, %s, %d) is reserved by the user and is never reassigned automatically", key.Stage, key.AppKey, key.Protocol, key.Port)
	}

	if port, ok := r.assigned[key]; ok {
		delete(r.assigned, key)
		delete(r.inUse, portClaim{key.Protocol, port})
	}

	return r.reserveFreeLocked(key)
}

// ReleaseAppFlow frees the ports an app holds under ONE of the two flows:
// compose reservations (service-qualified keys) or single-container ones
// (Service ""). An update that migrates an app between the flows must drop the
// reservations of the flow it is leaving — nothing will ever look those keys up
// again, so they would hold their pool ports until the agent restarts — without
// touching the ones the incoming flow has already made for itself.
func (r *HostPortRegistry) ReleaseAppFlow(stage common.Stage, appKey uint64, composeFlow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for key, port := range r.assigned {
		if key.Stage != stage || key.AppKey != appKey {
			continue
		}
		if (key.Service != "") != composeFlow {
			continue
		}
		delete(r.assigned, key)
		delete(r.inUse, portClaim{key.Protocol, port})
		delete(r.pinned, key)
	}
}

// ReleaseApp frees every port held by an app. Called on uninstall/remove
// only — stopped apps keep their reservations so restarts get the same port.
func (r *HostPortRegistry) ReleaseApp(stage common.Stage, appKey uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for key, port := range r.assigned {
		if key.Stage == stage && key.AppKey == appKey {
			delete(r.assigned, key)
			delete(r.inUse, portClaim{key.Protocol, port})
			delete(r.pinned, key)
		}
	}
}

// reservedByOtherLocked reports whether another app's rule has port pinned as
// its reserved host port. Callers must hold r.mu.
func (r *HostPortRegistry) reservedByOtherLocked(key hostPortKey, port uint64) bool {
	if r.reservations == nil {
		return false
	}
	for _, reservation := range r.reservations() {
		if reservation.Protocol == key.Protocol && reservation.Port == port && !sameRule(reservation.Owner, key) {
			return true
		}
	}
	return false
}

// reserveFreeLocked scans the pool for a port that neither the registry, a
// user reservation, nor the OS considers taken. Callers must hold r.mu; the
// lock is held across probe+record so concurrent reservations cannot pick the
// same port.
func (r *HostPortRegistry) reserveFreeLocked(key hostPortKey) (uint64, error) {
	var reserved []reservedHostPort
	if r.reservations != nil {
		reserved = r.reservations()
	}

	for port := hostPortRangeStart; port <= hostPortRangeEnd; port++ {
		if _, taken := r.inUse[portClaim{key.Protocol, port}]; taken {
			continue
		}
		reservedByOther := false
		for _, reservation := range reserved {
			if reservation.Protocol == key.Protocol && reservation.Port == port && !sameRule(reservation.Owner, key) {
				reservedByOther = true
				break
			}
		}
		if reservedByOther {
			continue
		}
		if !r.probeFree(key.Protocol, port) {
			continue
		}
		r.record(key, port)
		return port, nil
	}

	return 0, fmt.Errorf("no free host port available in range %d-%d", hostPortRangeStart, hostPortRangeEnd)
}

func (r *HostPortRegistry) record(key hostPortKey, port uint64) {
	r.assigned[key] = port
	r.inUse[portClaim{key.Protocol, port}] = struct{}{}
}

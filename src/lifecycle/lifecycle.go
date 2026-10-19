// Package lifecycle brokers process-restart requests between the components
// that need one (self-update activation, the system_restart_agent handler) and
// whatever supervises the process. Under the Windows service and under the
// Linux unit ironflock-device-agent.service, the control loop subscribes and
// turns each request into an exit-for-restart. A legacy Linux install
// (reagent.service + reagent-manager.sh) subscribes nothing, so RestartAgent
// execs systemctl there.
package lifecycle

import (
	"errors"
	"sync"
)

var (
	mu       sync.Mutex
	requests chan string
)

// ErrNotSupervised is returned by RequestRestart when no supervisor is
// subscribed: in console mode there is nothing that would start the process
// again, so callers must surface this instead of exiting.
var ErrNotSupervised = errors.New("no process supervisor available: the agent is not running as a service")

// Subscribe marks the process as supervised and returns the channel restart
// requests are delivered on. Called once by the service control loop.
func Subscribe() <-chan string {
	mu.Lock()
	defer mu.Unlock()

	if requests == nil {
		requests = make(chan string, 1)
	}
	return requests
}

// Supervised reports whether a supervisor is subscribed.
func Supervised() bool {
	mu.Lock()
	defer mu.Unlock()

	return requests != nil
}

// RequestRestart asks the supervisor to restart the process. A request that
// arrives while one is already pending is coalesced into it. Returns
// ErrNotSupervised when nothing is subscribed.
func RequestRestart(reason string) error {
	mu.Lock()
	defer mu.Unlock()

	if requests == nil {
		return ErrNotSupervised
	}

	select {
	case requests <- reason:
	default: // a restart is already pending; it covers this request too
	}
	return nil
}

var (
	readyOnce sync.Once
	ready     = make(chan struct{})
)

// MarkReady records that the agent finished its local start (database open,
// Docker client set up). Safe to call more than once.
func MarkReady() {
	readyOnce.Do(func() { close(ready) })
}

// Ready is closed once MarkReady has been called.
func Ready() <-chan struct{} {
	return ready
}

var connectedFunc func() bool

// SetConnectivity registers how to tell whether the agent's session to
// IronFlock is up (the WAMP session, once it exists).
func SetConnectivity(connected func() bool) {
	mu.Lock()
	defer mu.Unlock()
	connectedFunc = connected
}

// Connected reports whether the agent's session to IronFlock is up (false
// before the session exists).
func Connected() bool {
	mu.Lock()
	fn := connectedFunc
	mu.Unlock()
	return fn != nil && fn()
}

var layoutMigration func() (bool, error)

// SetLayoutMigration registers how a user's request starts the move of a
// legacy install to the current layout (a legacy agent registers it).
func SetLayoutMigration(start func() (bool, error)) {
	mu.Lock()
	defer mu.Unlock()
	layoutMigration = start
}

// StartLayoutMigration starts that move, if this agent can make one, and
// reports whether it started.
func StartLayoutMigration() (bool, error) {
	mu.Lock()
	start := layoutMigration
	mu.Unlock()
	if start == nil {
		return false, nil
	}
	return start()
}

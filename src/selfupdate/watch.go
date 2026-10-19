package selfupdate

import (
	"context"
	"time"
)

// Defaults of WatchConfig.
const (
	DefaultHealthyUptime = 2 * time.Minute
	// DefaultDeadline: a new version that is not ready and connected to
	// IronFlock this long after its start is rolled back.
	DefaultDeadline = 10 * time.Minute
	defaultPoll     = 5 * time.Second
	// recordEvery throttles the last-connected writes (an SD card's life).
	recordEvery = 10 * time.Minute
)

// WatchConfig is what Watch observes of the running agent.
type WatchConfig struct {
	Version string
	// Ready: the agent finished its local start (database, Docker client).
	Ready func() bool
	// Connected: the agent's session to IronFlock is up.
	Connected func() bool
	// OnConnected runs once, the first time Connected is true.
	OnConnected   func()
	HealthyUptime time.Duration
	Deadline      time.Duration
	Poll          time.Duration
}

// Watch decides, for the life of the process, what the running version's
// connection to IronFlock means for the update state:
//
//   - a new version in probation passes once it is ready, connected and up
//     HealthyUptime; one that is not ready and connected within Deadline is
//     rolled back (the returned channel then receives the reason, and the
//     caller must exit for a restart into the previous version);
//   - an old version running after a rollback judges the new one (Judge);
//   - the time of the last connection is kept on disk (LastConnected).
func (m *Manager) Watch(ctx context.Context, cfg WatchConfig) <-chan string {
	if cfg.HealthyUptime == 0 {
		cfg.HealthyUptime = DefaultHealthyUptime
	}
	if cfg.Deadline == 0 {
		cfg.Deadline = DefaultDeadline
	}
	if cfg.Poll == 0 {
		cfg.Poll = defaultPoll
	}
	restart := make(chan string, 1)
	w := &watcher{m: m, cfg: cfg, start: m.now()}
	go func() {
		ticker := time.NewTicker(cfg.Poll)
		defer ticker.Stop()
		for {
			if reason := w.step(m.now()); reason != "" {
				restart <- reason
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return restart
}

type watcher struct {
	m             *Manager
	cfg           WatchConfig
	start         time.Time
	ready         bool
	connectedEver bool
	lastRecorded  time.Time
}

// step looks once; it returns a restart reason after a rollback.
func (w *watcher) step(now time.Time) string {
	connected := w.cfg.Connected != nil && w.cfg.Connected()
	if connected {
		if !w.connectedEver && w.cfg.OnConnected != nil {
			w.cfg.OnConnected()
		}
		w.connectedEver = true
		if now.Sub(w.lastRecorded) >= recordEvery {
			w.m.RecordConnected(now)
			w.lastRecorded = now
		}
	}
	if w.cfg.Ready != nil && w.cfg.Ready() {
		w.ready = true
	}

	elapsed := now.Sub(w.start)
	if w.m.InProbation(w.cfg.Version) {
		switch {
		case w.ready && w.connectedEver && elapsed >= w.cfg.HealthyUptime:
			w.m.MarkHealthy()
		case elapsed >= w.cfg.Deadline && !(w.ready && w.connectedEver):
			if w.m.RollBackUnconnected(w.cfg.Version, w.cfg.Deadline).RollbackPerformed {
				return "v" + w.cfg.Version + " did not connect to IronFlock in time; rolled back"
			}
		}
	}
	proven := w.ready && w.connectedEver && elapsed >= w.cfg.HealthyUptime
	w.m.Judge(w.cfg.Version, proven, elapsed)
	return ""
}

const (
	// connectedRecently: the running version counts as proven while it was
	// connected this recently (selfupdate.Watch records it every minute).
	connectedRecently = recordEvery + 2*time.Minute
	// rescueAfter: a device disconnected this long tries a newer version
	// anyway — the running one may be what keeps it off IronFlock.
	rescueAfter = 24 * time.Hour
)

// ActivationHold says why an update must wait ("" when it may go ahead).
// Updates are activated from a connected version, so that the rollback
// target is one that is known to connect. A device that has been away for
// rescueAfter (or never connected) activates anyway: it has nothing to lose.
func (m *Manager) ActivationHold(now time.Time) string {
	last, ok := m.LastConnected()
	if !ok {
		return ""
	}
	away := now.Sub(last)
	if away < connectedRecently || away >= rescueAfter {
		return ""
	}
	return "the running version is not connected to IronFlock (an update is activated from a connected version; after " + rescueAfter.String() + " away, anyway)"
}

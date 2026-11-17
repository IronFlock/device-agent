package container

import (
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// WaitForDaemon's pace and log. The agent waits for Docker in a loop for as
// long as the daemon is away (agent.go), and on a Windows machine without
// Docker that is for good: one line and a ping per 100 ms filled the agent's
// log with nothing else. The pings back off to daemonRetryMax, and the log
// says when the daemon went away, how long it has been away every
// daemonDownLogInterval, and when it is back.
const (
	daemonRetryFirst      = 100 * time.Millisecond
	daemonRetryMax        = 2 * time.Second
	daemonDownLogInterval = 5 * time.Minute
)

// daemonWait keeps an outage across WaitForDaemon calls: the agent calls it
// again every 30 s while Docker is away, and the backoff and the log go on
// from where the last call left them. The zero value is ready to use.
type daemonWait struct {
	mu sync.Mutex
	// now and sleep are the clock; nil means time.Now and time.Sleep. A test
	// sets both.
	now   func() time.Time
	sleep func(time.Duration)

	downSince time.Time // zero while the daemon answers
	lastLog   time.Time
	failures  int
}

func (w *daemonWait) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *daemonWait) pause(d time.Duration) {
	if w.sleep != nil {
		w.sleep(d)
		return
	}
	time.Sleep(d)
}

// wait pings until ping succeeds or timeout has passed. The last ping is at
// the deadline: a daemon that comes up while the backoff sleeps is found
// before wait gives up.
func (w *daemonWait) wait(ping func() error, timeout time.Duration) error {
	deadline := w.clock().Add(timeout)
	for {
		err := ping()
		if err == nil {
			w.up()
			return nil
		}

		delay := w.down(err)
		remaining := deadline.Sub(w.clock())
		if remaining <= 0 {
			return errors.New("maxium retry timeout for docker daemon ping was exceeded")
		}
		if delay > remaining {
			delay = remaining
		}
		w.pause(delay)
	}
}

// down notes a failed ping, logs it when it starts an outage or when the
// last line is daemonDownLogInterval old, and returns the pause before the
// next ping.
func (w *daemonWait) down(err error) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.clock()
	if w.downSince.IsZero() {
		w.downSince = now
		w.lastLog = now
		w.failures = 0
		log.Warn().Err(err).Msg("Docker daemon is not reachable, retrying")
	} else if now.Sub(w.lastLog) >= daemonDownLogInterval {
		w.lastLog = now
		log.Warn().Err(err).Msgf("Docker daemon has not been reachable for %s, retrying", now.Sub(w.downSince).Round(time.Second))
	}

	delay := daemonRetryFirst
	for i := 0; i < w.failures && delay < daemonRetryMax; i++ {
		delay *= 2
	}
	if delay > daemonRetryMax {
		delay = daemonRetryMax
	}
	w.failures++
	return delay
}

// up notes a ping that succeeded and logs the end of an outage.
func (w *daemonWait) up() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.downSince.IsZero() {
		return
	}
	log.Info().Msgf("Docker daemon is reachable again after %s", w.clock().Sub(w.downSince).Round(time.Second))
	w.downSince = time.Time{}
	w.failures = 0
}

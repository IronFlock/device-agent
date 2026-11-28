package api

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/container"
	"reagent/messenger/topics"
	"reagent/safe"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// engineReadTimeout bounds one read of the Docker engine's platform and
// version. A var for the tests.
var engineReadTimeout = 10 * time.Second

// connectReportWait bounds how long a background report waits for a connect
// report in flight. A var for the tests.
var connectReportWait = 30 * time.Second

// deviceReportCalls lets one background device report at a time reach the
// platform: two at once race for the device's row, and the one that loses is
// dropped. The connect's report never takes it: the connect waits for nothing.
var deviceReportCalls sync.Mutex

// connectReports counts the connect's device reports in flight, which a
// resend waits for (bounded) so as not to race them. The connect only counts;
// it never waits on it.
var connectReports reportsInFlight

type reportsInFlight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

func (f *reportsInFlight) start() {
	f.mu.Lock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
	f.mu.Unlock()
}

func (f *reportsInFlight) done() {
	f.mu.Lock()
	f.n--
	if f.n == 0 {
		close(f.idle)
	}
	f.mu.Unlock()
}

// wait returns once no report is in flight, or after at most d.
func (f *reportsInFlight) wait(d time.Duration) {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return
	}
	idle := f.idle
	f.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	}
}

// EngineReport keeps what the device report says about the Docker engine that
// runs the apps: engine_platform and engine_version. The engine is read in
// the background, never on the way to a connection: a report built before
// the engine answered, or while Docker is absent or hung, simply has no such
// keys, and the platform keeps what it has stored.
//
// It is read when the agent starts, after every connect's report, when a
// Docker daemon that was not there at the start comes up, and when the daemon
// comes back after an outage. When what it knows differs from what the last
// report that went through carried, the report is sent again. A daemon that
// goes away and comes back with the same engine sends nothing; an outage is
// "not known", never a change.
type EngineReport struct {
	read func(ctx context.Context) (container.EngineInfo, error)

	mu sync.Mutex
	// known is the last engine read, field by field: a read that fails or
	// leaves a field out keeps the previous value.
	known container.EngineInfo
	// sent is what the platform got: the values of reports that went through.
	sent container.EngineInfo
	// connected is set once a connect's report was sent; before it, the
	// connect's report carries what is known and nothing is sent again.
	connected bool
	// One read (and send) at a time; a Refresh meanwhile makes it read once
	// more.
	reading bool
	again   bool
	resend  func()
}

// NewEngineReport returns an EngineReport that reads the engine with read.
func NewEngineReport(read func(ctx context.Context) (container.EngineInfo, error)) *EngineReport {
	return &EngineReport{read: read}
}

// SetResend sets what sends the device report again after a change.
func (e *EngineReport) SetResend(resend func()) {
	e.mu.Lock()
	e.resend = resend
	e.mu.Unlock()
}

// Refresh reads the engine, bounded by engineReadTimeout (but see
// container.Docker.EngineInfo), and sends the report again when the platform
// has not got what it knows. Errors are logged at debug level and change
// nothing. Run it in the background.
func (e *EngineReport) Refresh() {
	e.mu.Lock()
	if e.reading {
		e.again = true
		e.mu.Unlock()
		return
	}
	e.reading = true
	e.mu.Unlock()

	for {
		info, err := e.readOnce()
		if err != nil {
			log.Debug().Err(err).Msg("could not read the Docker engine's platform and version")
		}

		e.mu.Lock()
		if err == nil {
			if info.Platform != "" {
				e.known.Platform = info.Platform
			}
			if info.Version != "" {
				e.known.Version = info.Version
			}
		}
		changed := e.connected && e.known != e.sent
		known := e.known
		resend := e.resend
		e.mu.Unlock()

		// A Refresh asked for while this one reads or sends makes it read
		// once more afterwards, also when the send panics.
		if changed && resend != nil {
			log.Info().Str("engine_platform", known.Platform).Str("engine_version", known.Version).Msg("reporting the Docker engine")
			safe.Run(resend)
		}

		e.mu.Lock()
		again := e.again
		e.again = false
		if !again {
			e.reading = false
		}
		e.mu.Unlock()
		if !again {
			return
		}
	}
}

// readOnce is one bounded read; a panic in it is an error.
func (e *EngineReport) readOnce() (info container.EngineInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("reading the Docker engine panicked: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), engineReadTimeout)
	defer cancel()
	return e.read(ctx)
}

// addTo puts what is known about the engine into a device report.
func (e *EngineReport) addTo(payload common.Dict) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.known.Platform != "" {
		payload["engine_platform"] = e.known.Platform
	}
	if e.known.Version != "" {
		payload["engine_version"] = e.known.Version
	}
}

// delivered remembers the engine values of a report that went through.
func (e *EngineReport) delivered(payload common.Dict) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := payload["engine_platform"].(string); ok {
		e.sent.Platform = v
	}
	if v, ok := payload["engine_version"].(string); ok {
		e.sent.Version = v
	}
}

// SendConnectReport sends the device report of a connect. It waits for no
// other report and no Docker read: it only counts itself in flight, and
// afterwards starts a read of the engine in the background, so that an engine
// the start could not read, or one that changed, is reported.
func (ex *External) SendConnectReport() error {
	payload, err := ex.callConnectReport()
	if ex.Engine != nil {
		if err == nil {
			ex.Engine.delivered(payload)
		}
		ex.Engine.mu.Lock()
		ex.Engine.connected = true
		ex.Engine.mu.Unlock()
		safe.Go(ex.Engine.Refresh)
	}
	return err
}

// callConnectReport builds and sends the connect's report, counted in flight
// until the call returned.
func (ex *External) callConnectReport() (common.Dict, error) {
	connectReports.start()
	defer connectReports.done()
	payload := ex.DeviceReport()
	_, err := ex.Messenger.Call(context.Background(), topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil)
	return payload, err
}

// sendBackgroundReport sends a device report with the extra keys, one at a
// time with the other background reports. The payload is built and the
// timeout starts once it is this report's turn.
func (ex *External) sendBackgroundReport(timeout time.Duration, extra common.Dict) error {
	deviceReportCalls.Lock()
	defer deviceReportCalls.Unlock()
	payload := ex.DeviceReport()
	for k, v := range extra {
		payload[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := ex.Messenger.Call(ctx, topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil)
	if err == nil && ex.Engine != nil {
		ex.Engine.delivered(payload)
	}
	return err
}

// ResendDeviceReport sends the device report on its own, for an engine the
// platform has not got. It lets a connect's report in flight finish first
// (bounded by connectReportWait), since the later of two at once can be
// dropped. Errors are logged; the next read after a connect or a daemon
// transition tries again.
func (ex *External) ResendDeviceReport() {
	connectReports.wait(connectReportWait)
	if err := ex.sendBackgroundReport(osReleaseReportTimeout, nil); err != nil {
		log.Warn().Err(err).Msg("failed to report the Docker engine to the backend")
	}
}

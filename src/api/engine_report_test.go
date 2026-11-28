package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/container"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/release"
	"reagent/testutil/fakes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEngineRead answers every engine read with the answer set last, and
// counts the reads.
type fakeEngineRead struct {
	mu     sync.Mutex
	answer engineAnswer
	reads  int
}

type engineAnswer struct {
	info container.EngineInfo
	err  error
	// hang makes the read wait for its context, as a hung daemon does.
	hang bool
	// wait makes the read wait for the channel to close.
	wait chan struct{}
	// panics makes the read panic.
	panics bool
}

func (f *fakeEngineRead) set(answer engineAnswer) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

func (f *fakeEngineRead) read(ctx context.Context) (container.EngineInfo, error) {
	f.mu.Lock()
	answer := f.answer
	f.reads++
	f.mu.Unlock()
	if answer.panics {
		panic("boom")
	}
	if answer.hang {
		<-ctx.Done()
		return container.EngineInfo{}, ctx.Err()
	}
	if answer.wait != nil {
		<-answer.wait
	}
	return answer.info, answer.err
}

func (f *fakeEngineRead) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// reportMessenger records the device reports, can fail them, can hold one
// call until released, and notes whether a call overlapped another and
// whether its context had run out when it was made.
type reportMessenger struct {
	*fakes.Messenger

	mu       sync.Mutex
	err      error
	block    chan struct{}
	inFlight int
	calls    []reportCall
}

type reportCall struct {
	payload    common.Dict
	overlapped bool
	ctxErr     error
}

func (r *reportMessenger) Call(ctx context.Context, topic topics.Topic, args []interface{}, _ common.Dict, _ common.Dict, _ func(messenger.Result)) (messenger.Result, error) {
	r.mu.Lock()
	block := r.block
	r.block = nil
	call := reportCall{payload: args[0].(common.Dict), overlapped: r.inFlight > 0, ctxErr: ctx.Err()}
	if topic == topics.UpdateDeviceArchitecture {
		r.calls = append(r.calls, call)
	}
	r.inFlight++
	err := r.err
	r.mu.Unlock()

	if block != nil {
		<-block
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	if call.ctxErr != nil {
		return messenger.Result{}, call.ctxErr
	}
	return messenger.Result{}, err
}

func (r *reportMessenger) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// holdNext makes the next call wait until the returned func is called.
func (r *reportMessenger) holdNext() (release func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	r.block = ch
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (r *reportMessenger) reports() []reportCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reportCall(nil), r.calls...)
}

func (r *reportMessenger) count() int { return len(r.reports()) }

func engineExternal(t *testing.T, answer engineAnswer) (*External, *reportMessenger, *fakeEngineRead) {
	t.Helper()
	read := &fakeEngineRead{answer: answer}
	engine := NewEngineReport(read.read)
	m := &reportMessenger{Messenger: fakes.NewMessenger()}
	ex := &External{Config: testConfig(), Messenger: m, Engine: engine}
	engine.SetResend(ex.ResendDeviceReport)
	return ex, m, read
}

// settle waits until the engine has been read reads times and no read runs.
func settle(t *testing.T, ex *External, read *fakeEngineRead, reads int) {
	t.Helper()
	require.Eventually(t, func() bool {
		ex.Engine.mu.Lock()
		reading := ex.Engine.reading
		ex.Engine.mu.Unlock()
		return read.count() >= reads && !reading
	}, 5*time.Second, time.Millisecond)
}

var (
	armv7Engine   = container.EngineInfo{Platform: "linux/arm/v7", Version: "28.5.2"}
	arm64Engine   = container.EngineInfo{Platform: "linux/arm64", Version: "29.1.0"}
	errDockerDown = errors.New("cannot connect to the Docker daemon")
)

func TestDeviceReportCarriesTheEngine(t *testing.T) {
	ex, m, _ := engineExternal(t, engineAnswer{info: arm64Engine})

	ex.Engine.Refresh()
	report := ex.DeviceReport()

	assert.Equal(t, "linux/arm64", report["engine_platform"])
	assert.Equal(t, "29.1.0", report["engine_version"])
	// Today's value stays the agent's own build, next to the engine's: a
	// 32-bit agent build next to an arm64 engine reports both.
	sysInfo := release.GetSystemInfo()
	assert.Equal(t, sysInfo.Arch+sysInfo.Variant, report["architecture"])
	assert.Zero(t, m.count(), "a read before the connect sends nothing of its own")
}

func TestDeviceReportLeavesAnUnknownEngineOut(t *testing.T) {
	for name, answer := range map[string]engineAnswer{
		"Docker absent":         {err: errDockerDown},
		"Docker hangs":          {hang: true},
		"version only":          {info: container.EngineInfo{Version: "28.5.2"}},
		"nothing read yet, nil": {},
	} {
		t.Run(name, func(t *testing.T) {
			restore := engineReadTimeout
			engineReadTimeout = 50 * time.Millisecond
			t.Cleanup(func() { engineReadTimeout = restore })
			ex, m, _ := engineExternal(t, answer)
			if name == "nothing read yet, nil" {
				ex.Engine = nil
			} else {
				ex.Engine.Refresh()
			}

			report := ex.DeviceReport()

			assert.NotContains(t, report, "engine_platform")
			if name == "version only" {
				assert.Equal(t, "28.5.2", report["engine_version"])
			} else {
				assert.NotContains(t, report, "engine_version")
			}
			assert.Zero(t, m.count())
		})
	}
}

// A hung daemon holds up no report: the connect's report goes out at once,
// without the engine, while the read waits out its timeout.
func TestHungDockerHoldsUpNoReport(t *testing.T) {
	restore := engineReadTimeout
	engineReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { engineReadTimeout = restore })
	ex, m, read := engineExternal(t, engineAnswer{hang: true})

	done := make(chan struct{})
	go func() { ex.Engine.Refresh(); close(done) }()
	require.Eventually(t, func() bool { return read.count() == 1 }, time.Second, time.Millisecond)

	start := time.Now()
	require.NoError(t, ex.SendConnectReport())
	assert.Less(t, time.Since(start), 100*time.Millisecond)
	require.Equal(t, 1, m.count())
	assert.NotContains(t, m.reports()[0].payload, "engine_platform")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not give up after its timeout")
	}
	settle(t, ex, read, 2)
	assert.Equal(t, 1, m.count(), "an engine that never answered changes nothing")
}

// The start could not read the engine (dockerd still starting), and the fast
// path never reads it again: the read after the connect's report does, and
// the report is sent again.
func TestConnectReadsAnEngineTheStartMissed(t *testing.T) {
	ex, m, read := engineExternal(t, engineAnswer{err: errDockerDown})
	ex.Engine.Refresh()

	read.set(engineAnswer{info: armv7Engine})
	require.NoError(t, ex.SendConnectReport())
	settle(t, ex, read, 2)

	require.Eventually(t, func() bool { return m.count() == 2 }, 5*time.Second, time.Millisecond)
	reports := m.reports()
	assert.NotContains(t, reports[0].payload, "engine_platform", "the connect's report went without it")
	assert.Equal(t, "linux/arm/v7", reports[1].payload["engine_platform"])
	assert.Equal(t, "28.5.2", reports[1].payload["engine_version"])
	assert.Contains(t, reports[1].payload, "architecture")
	assert.Contains(t, reports[1].payload, "device_key")
}

// One report per change: a daemon that flaps with the same engine sends
// nothing, another engine is sent once.
func TestEngineChangeSendsTheReportOnce(t *testing.T) {
	ex, m, read := engineExternal(t, engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()
	require.NoError(t, ex.SendConnectReport())
	settle(t, ex, read, 2)
	require.Equal(t, 1, m.count())
	assert.Equal(t, "linux/arm/v7", m.reports()[0].payload["engine_platform"])

	read.set(engineAnswer{err: errDockerDown})
	ex.Engine.Refresh()
	read.set(engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()
	assert.Equal(t, 1, m.count(), "an outage and the same engine again send nothing")
	assert.Equal(t, "linux/arm/v7", ex.DeviceReport()["engine_platform"], "an outage keeps what is known")

	read.set(engineAnswer{info: arm64Engine})
	ex.Engine.Refresh()
	require.Equal(t, 2, m.count())
	assert.Equal(t, "linux/arm64", m.reports()[1].payload["engine_platform"])

	ex.Engine.Refresh()
	assert.Equal(t, 2, m.count())
}

// A report that did not go through does not count as sent: the next read
// sends it again, until one goes through.
func TestFailedReportIsSentAgain(t *testing.T) {
	ex, m, read := engineExternal(t, engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()
	m.setErr(errors.New("dropped"))
	require.Error(t, ex.SendConnectReport())
	settle(t, ex, read, 2) // the resend after the connect fails as well
	require.Equal(t, 2, m.count())

	m.setErr(nil)
	ex.Engine.Refresh() // e.g. the daemon's next transition, or the next connect
	require.Equal(t, 3, m.count())
	assert.Equal(t, "linux/arm/v7", m.reports()[2].payload["engine_platform"])

	ex.Engine.Refresh()
	assert.Equal(t, 3, m.count(), "once it went through, nothing more")
}

// A resend waits for a connect's report in flight, so that the two do not
// race for the device's row; the connect's report waits for nothing.
func TestResendWaitsForTheConnectReport(t *testing.T) {
	ex, m, read := engineExternal(t, engineAnswer{err: errDockerDown})
	ex.Engine.Refresh()
	release := m.holdNext()
	t.Cleanup(release)

	connected := make(chan error, 1)
	go func() { connected <- ex.SendConnectReport() }()
	require.Eventually(t, func() bool { return m.count() == 1 }, time.Second, time.Millisecond)

	// The engine becomes known and a reconnect's earlier session counts as
	// connected: a read now resends.
	ex.Engine.mu.Lock()
	ex.Engine.connected = true
	ex.Engine.mu.Unlock()
	read.set(engineAnswer{info: armv7Engine})
	resent := make(chan struct{})
	go func() { ex.Engine.Refresh(); close(resent) }()

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, m.count(), "the resend waits while the connect's report is in flight")
	release()
	require.NoError(t, <-connected)
	<-resent

	reports := m.reports()
	require.GreaterOrEqual(t, len(reports), 2)
	for _, r := range reports {
		assert.False(t, r.overlapped, "no two reports at once")
	}
}

// The wait for a connect's report is bounded.
func TestResendWaitIsBounded(t *testing.T) {
	restore := connectReportWait
	connectReportWait = 50 * time.Millisecond
	t.Cleanup(func() { connectReportWait = restore })
	ex, m, _ := engineExternal(t, engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()
	release := m.holdNext()
	t.Cleanup(release)
	go func() { _ = ex.SendConnectReport() }()
	require.Eventually(t, func() bool { return m.count() == 1 }, time.Second, time.Millisecond)

	start := time.Now()
	ex.ResendDeviceReport()

	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, 2, m.count())
}

// The connect's report takes no lock of the background reports.
func TestConnectReportWaitsForNoBackgroundReport(t *testing.T) {
	ex, m, _ := engineExternal(t, engineAnswer{err: errDockerDown})
	deviceReportCalls.Lock()
	t.Cleanup(deviceReportCalls.Unlock)

	start := time.Now()
	require.NoError(t, ex.SendConnectReport())

	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 1, m.count())
}

// A background report's timeout starts when it is its turn, and its payload
// is built then: one that waited behind another neither times out for it nor
// sends what was known before it waited.
func TestBackgroundReportWaitsItsTurn(t *testing.T) {
	ex, m, read := engineExternal(t, engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()

	deviceReportCalls.Lock()
	sent := make(chan error, 1)
	go func() { sent <- ex.sendBackgroundReport(50*time.Millisecond, common.Dict{"os_release": common.Dict{}}) }()
	time.Sleep(150 * time.Millisecond) // longer than its timeout
	read.set(engineAnswer{info: arm64Engine})
	ex.Engine.Refresh()
	deviceReportCalls.Unlock()

	require.NoError(t, <-sent)
	require.Equal(t, 1, m.count())
	report := m.reports()[0]
	assert.NoError(t, report.ctxErr)
	assert.Equal(t, "linux/arm64", report.payload["engine_platform"])
	assert.Contains(t, report.payload, "os_release")
}

// Refreshes during a read do not pile up: they make the read run once more.
func TestRefreshesDuringAReadCoalesce(t *testing.T) {
	wait := make(chan struct{})
	ex, _, read := engineExternal(t, engineAnswer{info: armv7Engine, wait: wait})

	done := make(chan struct{})
	go func() { ex.Engine.Refresh(); close(done) }()
	require.Eventually(t, func() bool { return read.count() == 1 }, time.Second, time.Millisecond)
	read.set(engineAnswer{info: arm64Engine})
	for range 5 {
		ex.Engine.Refresh()
	}
	close(wait)
	<-done

	assert.Equal(t, 2, read.count())
	assert.Equal(t, "linux/arm64", ex.DeviceReport()["engine_platform"], "the read after the others sees the latest engine")
}

// A read that panics is an error, and the reads after it go on.
func TestRefreshAfterAPanickingRead(t *testing.T) {
	ex, _, read := engineExternal(t, engineAnswer{panics: true})
	assert.NotPanics(t, ex.Engine.Refresh)

	read.set(engineAnswer{info: armv7Engine})
	ex.Engine.Refresh()

	assert.Equal(t, "linux/arm/v7", ex.DeviceReport()["engine_platform"])
}

// A resend that panics keeps the read that was asked for meanwhile.
func TestRefreshKeepsAPendingReadWhenTheResendPanics(t *testing.T) {
	wait := make(chan struct{})
	ex, _, read := engineExternal(t, engineAnswer{info: armv7Engine, wait: wait})
	ex.Engine.connected = true
	ex.Engine.SetResend(func() { panic("boom") })

	done := make(chan struct{})
	go func() { ex.Engine.Refresh(); close(done) }()
	require.Eventually(t, func() bool { return read.count() == 1 }, time.Second, time.Millisecond)
	ex.Engine.Refresh()
	read.set(engineAnswer{info: arm64Engine})
	close(wait)
	<-done

	assert.Equal(t, 2, read.count())
	assert.Equal(t, "linux/arm64", ex.DeviceReport()["engine_platform"])
}

package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reagent/common"
	"reagent/errdefs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironflock/nexus/v3/client"
	"github.com/ironflock/nexus/v3/wamp"
	pkgerrors "github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	zerologpkgerrors "github.com/rs/zerolog/pkgerrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// TestWampSession_Connected - Tests for connection state
// =============================================================================

func TestWampSession_Connected(t *testing.T) {
	t.Run("returns true when connected", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		assert.True(t, session.Connected())
	})

	t.Run("returns false after close", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)

		session.Close()
		assert.False(t, session.Connected())
	})
}

// =============================================================================
// TestWampSession_Close - Tests for session close behavior
// =============================================================================

func TestWampSession_Close(t *testing.T) {
	t.Run("Close is idempotent", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)

		assert.NotPanics(t, func() {
			session.Close()
			session.Close()
			session.Close()
		})
	})

	t.Run("Connected returns false after Close", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)

		session.Close()
		assert.False(t, session.Connected())
	})
}

// =============================================================================
// TestWampSession_ThreadSafety - Tests for concurrent access safety
// =============================================================================

func TestWampSession_ThreadSafety(t *testing.T) {
	t.Run("concurrent access to Connected is safe", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = session.Connected()
			}()
		}
		wg.Wait()
	})

	t.Run("concurrent Close calls are safe", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)

		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				session.Close()
			}()
		}
		wg.Wait()
	})
}

// =============================================================================
// TestOfflineMessenger - Tests for offline mode
// =============================================================================

func TestOfflineMessenger(t *testing.T) {
	t.Run("NewOffline creates messenger with config", func(t *testing.T) {
		cfg := testConfig()
		messenger := NewOffline(cfg)

		require.NotNil(t, messenger)
		assert.Equal(t, cfg, messenger.GetConfig())
	})

	t.Run("Connected returns false", func(t *testing.T) {
		messenger := NewOffline(testConfig())
		assert.False(t, messenger.Connected())
	})

	t.Run("GetSessionID returns 0", func(t *testing.T) {
		messenger := NewOffline(testConfig())
		assert.Equal(t, uint64(0), messenger.GetSessionID())
	})

	t.Run("Publish does not error", func(t *testing.T) {
		messenger := NewOffline(testConfig())
		err := messenger.Publish("test.topic", nil, nil, nil)
		assert.NoError(t, err)
	})

	t.Run("Register does not error", func(t *testing.T) {
		messenger := NewOffline(testConfig())
		err := messenger.Register("test.topic", nil, nil)
		assert.NoError(t, err)
	})

	t.Run("Subscribe does not error", func(t *testing.T) {
		messenger := NewOffline(testConfig())
		err := messenger.Subscribe("test.topic", nil, nil)
		assert.NoError(t, err)
	})
}

// =============================================================================
// TestDeviceStatus - Tests for device status constants
// =============================================================================

func TestDeviceStatus(t *testing.T) {
	t.Run("DeviceStatus constants have correct values", func(t *testing.T) {
		assert.Equal(t, DeviceStatus("CONNECTED"), CONNECTED)
		assert.Equal(t, DeviceStatus("DISCONNECTED"), DISCONNECTED)
		assert.Equal(t, DeviceStatus("CONFIGURING"), CONFIGURING)
	})
}

// =============================================================================
// TestErrNotConnected - Tests for error constants
// =============================================================================

func TestErrNotConnected(t *testing.T) {
	t.Run("ErrNotConnected has expected message", func(t *testing.T) {
		assert.Equal(t, "not connected", ErrNotConnected.Error())
	})
}

// =============================================================================
// TestSocketConfig - Tests for socket configuration
// =============================================================================

func TestSocketConfig(t *testing.T) {
	t.Run("default values are zero", func(t *testing.T) {
		cfg := SocketConfig{}
		assert.Equal(t, time.Duration(0), cfg.PingPongTimeout)
		assert.Equal(t, time.Duration(0), cfg.ResponseTimeout)
		assert.Equal(t, time.Duration(0), cfg.ConnectionTimeout)
		assert.False(t, cfg.SetupTestament)
	})
}

// =============================================================================
// TestWampSession_OnConnectCallback - Tests for connection callbacks
// =============================================================================

func TestWampSession_OnConnectCallback(t *testing.T) {
	t.Run("SetOnConnect stores callback", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		called := false
		session.SetOnConnect(func(reconnect bool) {
			called = true
		})

		session.mu.Lock()
		cb := session.onConnect
		session.mu.Unlock()

		require.NotNil(t, cb)
		cb(true)
		assert.True(t, called)
	})

	t.Run("SetOnConnect can be called multiple times", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		callCount := 0
		session.SetOnConnect(func(reconnect bool) { callCount = 1 })
		session.SetOnConnect(func(reconnect bool) { callCount = 2 })

		session.mu.Lock()
		cb := session.onConnect
		session.mu.Unlock()

		cb(false)
		assert.Equal(t, 2, callCount, "second callback should override first")
	})

	t.Run("callback receives correct reconnect flag", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		var receivedReconnect bool
		session.SetOnConnect(func(reconnect bool) {
			receivedReconnect = reconnect
		})

		session.mu.Lock()
		cb := session.onConnect
		session.mu.Unlock()

		cb(true)
		assert.True(t, receivedReconnect)

		cb(false)
		assert.False(t, receivedReconnect)
	})
}

// =============================================================================
// TestWampSession_ConnectionRetries - Tests for connection retry behavior
// =============================================================================

func TestWampSession_ConnectionRetries(t *testing.T) {
	t.Run("retries on connection error until successful", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient().SetConnectFailCount(2)

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		assert.Equal(t, 3, mockClient.ConnectAttempts())
		assert.True(t, session.Connected())
	})

	t.Run("provider receives correct URL and config", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.DeviceEndpointURL = "wss://test.example.com/ws"

		socketConfig := &SocketConfig{
			ConnectionTimeout: time.Millisecond * 100,
			ResponseTimeout:   time.Second * 5,
		}

		var mu sync.Mutex
		var receivedURL string
		var receivedConfig client.Config
		mockClient := NewMockClient()

		provider := func(ctx context.Context, url string, cfg client.Config) (NexusClient, error) {
			mu.Lock()
			receivedURL = url
			receivedConfig = cfg
			mu.Unlock()
			return mockClient.ConnectNet(ctx, url, cfg)
		}

		session, err := NewWampSession(cfg, socketConfig, nil, provider)
		require.NoError(t, err)
		defer session.Close()

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "wss://test.example.com/ws", receivedURL)
		assert.Equal(t, "realm1", receivedConfig.Realm)
		assert.Equal(t, socketConfig.ResponseTimeout, receivedConfig.ResponseTimeout)
	})
}

// =============================================================================
// TestWampSession_Reconnection - Tests for automatic reconnection
// =============================================================================

func TestWampSession_Reconnection(t *testing.T) {
	t.Run("triggers reconnection when client disconnects", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		// Use SimulateConnectionDrop to trigger reconnection
		client := mockClient.LastClient()
		client.SimulateConnectionDrop()
		assert.False(t, session.Connected())
		time.Sleep(500 * time.Millisecond)

		assert.True(t, session.Connected())
	})

	t.Run("invokes onConnect callback after reconnection", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		var callbackMu sync.Mutex
		var callbackCalled bool
		var wasReconnect bool
		session.SetOnConnect(func(reconnect bool) {
			callbackMu.Lock()
			callbackCalled = true
			wasReconnect = reconnect
			callbackMu.Unlock()
		})

		client := mockClient.LastClient()
		client.SimulateConnectionDrop()

		time.Sleep(500 * time.Millisecond)

		callbackMu.Lock()
		defer callbackMu.Unlock()
		assert.True(t, callbackCalled)
		assert.True(t, wasReconnect)
	})

	t.Run("survives multiple sequential disconnects", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		for i := 0; i < 5; i++ {
			require.Eventually(t, func() bool {
				c := mockClient.LastClient()
				return c != nil && c.Connected()
			}, time.Second, 5*time.Millisecond, "cycle %d: expected fresh connected client", i)
			mockClient.LastClient().SimulateConnectionDrop()
		}

		require.Eventually(t, session.Connected, time.Second, 5*time.Millisecond,
			"session should reconnect after final drop")
		assert.GreaterOrEqual(t, mockClient.ClientCount(), 6, "expected initial + 5 reconnects")
	})

	t.Run("recovers when dial fails repeatedly then succeeds", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}

		mockClient := NewMockClient()
		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		// Make every subsequent ConnectNet fail several times before succeeding.
		mockClient.SetConnectFailCount(3)
		mockClient.LastClient().SimulateConnectionDrop()

		require.Eventually(t, session.Connected, 30*time.Second, 50*time.Millisecond,
			"session must reconnect even after repeated dial failures")
	})

	t.Run("panicking onConnect callback does not stop the session", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		defer session.Close()

		var panicCount int32
		session.SetOnConnect(func(reconnect bool) {
			atomic.AddInt32(&panicCount, 1)
			panic("intentional test panic in onConnect")
		})

		// Two consecutive disconnects: each triggers a panicking callback,
		// the watcher must survive both and keep the session online.
		for i := 0; i < 2; i++ {
			require.Eventually(t, func() bool {
				c := mockClient.LastClient()
				return c != nil && c.Connected()
			}, time.Second, 5*time.Millisecond)
			mockClient.LastClient().SimulateConnectionDrop()
		}

		require.Eventually(t, session.Connected, time.Second, 5*time.Millisecond,
			"session must stay reconnectable even when callback panics")
		// connect() runs onConnect in its own goroutine, so the callback for the
		// last reconnect may still be pending when Connected() turns true.
		require.Eventually(t, func() bool { return atomic.LoadInt32(&panicCount) >= 2 },
			time.Second, 5*time.Millisecond, "callback should have been invoked for each reconnect")
	})

	t.Run("Close stops the reconnect loop even mid-dial", func(t *testing.T) {
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}

		mockClient := NewMockClient()
		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)

		// Force the next dial to keep failing so the reconnect loop is busy.
		mockClient.SetConnectFailCount(1000)
		mockClient.LastClient().SimulateConnectionDrop()

		// Give the loop a moment to enter the failing-dial state, then close.
		time.Sleep(50 * time.Millisecond)
		closed := make(chan struct{})
		go func() {
			session.Close()
			close(closed)
		}()

		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("Close blocked while dial loop was active")
		}
		assert.False(t, session.Connected())
	})
}

// =============================================================================
// UpdateRemoteDeviceStatus — heartbeat payload shape
//
// docker_available rides INSIDE the stats dict (persisted verbatim to the
// device row by the backend, returned on cold load), tunnel_capable rides at
// the top level (dedicated column). Both are omitted entirely until wired, so
// a fleet of older agents never blanks the stored values.
// =============================================================================

func TestUpdateRemoteDeviceStatusPayload(t *testing.T) {
	newSession := func(t *testing.T) (*WampSession, *MockClient) {
		t.Helper()
		cfg := testConfig()
		socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
		mockClient := NewMockClient()

		session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
		require.NoError(t, err)
		t.Cleanup(session.Close)
		return session, mockClient
	}

	payloadOf := func(t *testing.T, mockClient *MockClient) map[string]interface{} {
		t.Helper()
		require.NotNil(t, mockClient.LastClient())
		args := mockClient.LastClient().LastCallArgs()
		require.NotEmpty(t, args)
		payload, ok := args[0].(common.Dict)
		require.True(t, ok, "status payload must be a dict")
		return payload
	}

	t.Run("carries docker_available inside stats when wired", func(t *testing.T) {
		session, mockClient := newSession(t)
		session.SetDockerAvailableFunc(func() bool { return false })

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		stats, ok := payloadOf(t, mockClient)["stats"].(common.Dict)
		require.True(t, ok, "payload must carry a stats dict")
		assert.Equal(t, false, stats["docker_available"])
	})

	t.Run("omits docker_available when not wired", func(t *testing.T) {
		session, mockClient := newSession(t)

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		stats, ok := payloadOf(t, mockClient)["stats"].(common.Dict)
		require.True(t, ok)
		_, present := stats["docker_available"]
		assert.False(t, present, "unwired agents must not send the field at all")
	})

	t.Run("keeps tunnel_capable at the top level", func(t *testing.T) {
		session, mockClient := newSession(t)
		session.SetTunnelCapableFunc(func() bool { return true })
		session.SetDockerAvailableFunc(func() bool { return true })

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		payload := payloadOf(t, mockClient)
		assert.Equal(t, true, payload["tunnel_capable"])
		_, topLevel := payload["docker_available"]
		assert.False(t, topLevel, "docker_available must live in stats, not top level")

		stats, ok := payload["stats"].(common.Dict)
		require.True(t, ok)
		assert.Equal(t, true, stats["docker_available"])
	})
}

// =============================================================================
// Registrations and subscriptions log their failures a few times a period
//
// A handler runs once per call or event, so whoever may call the procedure or
// publish to the topic decides how often it fails: every privilege check
// refuses the callers it does not know, a terminal refuses every keystroke from
// someone who does not own it. Logging each failure would let that someone fill
// the device's log and rotate its history away. What is not logged is counted,
// and the count is logged once the period is over.
// =============================================================================

// eventClient keeps each subscription's event handler and each registration's
// invocation handler, so a test can deliver events and calls the way the
// router would.
type eventClient struct {
	*MockNexusClient

	mu         sync.Mutex
	handlers   map[string]client.EventHandler
	procedures map[string]client.InvocationHandler
}

func newEventClient() *eventClient {
	return &eventClient{
		MockNexusClient: &MockNexusClient{connected: true, done: make(chan struct{})},
		handlers:        map[string]client.EventHandler{},
		procedures:      map[string]client.InvocationHandler{},
	}
}

func (c *eventClient) Subscribe(topic string, fn client.EventHandler, options wamp.Dict) error {
	c.mu.Lock()
	c.handlers[topic] = fn
	c.mu.Unlock()

	return c.MockNexusClient.Subscribe(topic, fn, options)
}

func (c *eventClient) Register(procedure string, fn client.InvocationHandler, options wamp.Dict) error {
	c.mu.Lock()
	c.procedures[procedure] = fn
	c.mu.Unlock()

	return c.MockNexusClient.Register(procedure, fn, options)
}

func (c *eventClient) deliver(t *testing.T, topic string, times int) {
	t.Helper()

	c.mu.Lock()
	handler := c.handlers[topic]
	c.mu.Unlock()
	require.NotNil(t, handler, "nothing subscribed %s", topic)

	for i := 0; i < times; i++ {
		handler(&wamp.Event{
			Subscription: 1,
			Publication:  wamp.ID(i + 1),
			Details:      wamp.Dict{"publisher_authid": "777"},
			Arguments:    wamp.List{"rm -rf / #"},
		})
	}
}

// invoke calls procedure as caller on a goroutine of its own, as the client
// does, and returns the answer.
func (c *eventClient) invoke(t *testing.T, procedure string, caller string, args wamp.List) client.InvokeResult {
	t.Helper()

	c.mu.Lock()
	handler := c.procedures[procedure]
	c.mu.Unlock()
	require.NotNil(t, handler, "nothing registered %s", procedure)

	answer := make(chan client.InvokeResult, 1)
	go func() {
		answer <- handler(context.Background(), &wamp.Invocation{
			Request:   1,
			Details:   wamp.Dict{"caller_authid": caller},
			Arguments: args,
		})
	}()

	select {
	case res := <-answer:
		return res
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the handler did not answer", procedure)
		return client.InvokeResult{}
	}
}

// failureLogChild tells the test binary to run a flood itself, in a process of
// its own: log.Logger is global, and the sessions of other tests still log from
// their goroutines, so swapping it here would race them.
const failureLogChild = "REAGENT_FAILURE_LOG_CHILD"

// childLog is where a child's logger writes besides stderr, so its flood can
// wait for the counts logged once a period is over.
type childLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *childLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// counted reports whether a count of the failures what names has been logged.
func (l *childLog) counted(what string) bool {
	return l.countsLogged(what) > 0
}

// countsLogged returns how many counts of the failures what names have been
// logged.
func (l *childLog) countsLogged(what string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	logged := 0
	for _, line := range decodeLogLines(l.buf.String()) {
		if _, ok := line["suppressed"]; ok && strings.Contains(line["message"].(string), what) {
			logged++
		}
	}
	return logged
}

// decodeLogLines decodes the JSON lines zerolog wrote, skipping the test
// binary's own output.
func decodeLogLines(logged string) []map[string]interface{} {
	var lines []map[string]interface{}
	for _, raw := range strings.Split(logged, "\n") {
		var line map[string]interface{}
		if json.Unmarshal([]byte(raw), &line) == nil {
			lines = append(lines, line)
		}
	}
	return lines
}

// logsOfChild runs the calling test in a child test binary, where it floods,
// and returns the lines the child logged. The child runs flood with its
// logger writing JSON to stderr and to logged, and logsOfChild returns nil
// there; the caller then returns too.
func logsOfChild(t *testing.T, flood func(t *testing.T, logged *childLog)) []map[string]interface{} {
	t.Helper()

	if os.Getenv(failureLogChild) == "1" {
		logged := &childLog{}
		log.Logger = zerolog.New(io.MultiWriter(os.Stderr, logged))
		zerolog.ErrorStackMarshaler = zerologpkgerrors.MarshalStack
		flood(t, logged)
		return nil
	}

	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = append(os.Environ(), failureLogChild+"=1")
	var logged, output bytes.Buffer
	child.Stderr = &logged
	child.Stdout = &output
	require.NoError(t, child.Run(), "%s\n%s", output.String(), logged.String())

	return decodeLogLines(logged.String())
}

// withMessage returns the lines logged with message.
func withMessage(lines []map[string]interface{}, message string) []map[string]interface{} {
	var matching []map[string]interface{}
	for _, line := range lines {
		if line["message"] == message {
			matching = append(matching, line)
		}
	}
	return matching
}

// countLines returns the lines that logged a count of the failures what names.
func countLines(lines []map[string]interface{}, what string) []map[string]interface{} {
	var found []map[string]interface{}
	for _, line := range lines {
		_, ok := line["suppressed"].(float64)
		if ok && strings.Contains(line["message"].(string), what+" were not logged") {
			found = append(found, line)
		}
	}
	return found
}

// counts returns the suppressed counts logged for the failures what names.
func counts(lines []map[string]interface{}, what string) []float64 {
	var found []float64
	for _, line := range countLines(lines, what) {
		found = append(found, line["suppressed"].(float64))
	}
	return found
}

// assertCountsWarn asserts the counts of the failures what names are logged
// at warn: a flood has to show at the level a device logs at.
func assertCountsWarn(t *testing.T, lines []map[string]interface{}, what string) {
	t.Helper()
	for _, line := range countLines(lines, what) {
		assert.Equal(t, "warn", line["level"], "the count of the %s", what)
	}
}

// testFailureLogPeriod is long enough that a flood fits in one period even
// under the race detector, and short enough to wait for its count.
const testFailureLogPeriod = time.Second

func TestSubscribeLogsCallbackErrorsAFewTimesAPeriod(t *testing.T) {
	lines := logsOfChild(t, floodSubscriptions)
	if lines == nil {
		return
	}

	about := func(topic string) []map[string]interface{} {
		return withMessage(lines, "An error occured during the subscribe result of "+topic)
	}

	assert.Len(t, about("re.mgmt.SER-1.term_write.flooded"), 5, "a flood logs a burst, not every event")
	assert.Len(t, about("re.mgmt.SER-1.term_write.other"), 3, "a flood of one subscription silences no other")
	assert.Empty(t, about("re.mgmt.SER-1.term_write.fine"))

	assert.Equal(t, []float64{195}, counts(lines, "failed events of re.mgmt.SER-1.term_write.flooded"),
		"the flood is counted once its period is over")
	assertCountsWarn(t, lines, "failed events of re.mgmt.SER-1.term_write.flooded")
	assert.Empty(t, counts(lines, "re.mgmt.SER-1.term_write.other"), "nothing of it went unlogged")
}

// floodSubscriptions refuses 200 events on one subscription and 3 on another,
// and serves 10 on a third.
func floodSubscriptions(t *testing.T, logged *childLog) {
	nc := newEventClient()
	session := &WampSession{client: nc, failureLogPeriod: testFailureLogPeriod}

	refuse := func(Result) error {
		return errors.New("dropped keystrokes from a publisher that does not own the terminal")
	}
	require.NoError(t, session.Subscribe("re.mgmt.SER-1.term_write.flooded", refuse, nil))
	require.NoError(t, session.Subscribe("re.mgmt.SER-1.term_write.other", refuse, nil))
	require.NoError(t, session.Subscribe("re.mgmt.SER-1.term_write.fine", func(Result) error { return nil }, nil))

	nc.deliver(t, "re.mgmt.SER-1.term_write.flooded", 200)
	nc.deliver(t, "re.mgmt.SER-1.term_write.other", 3)
	nc.deliver(t, "re.mgmt.SER-1.term_write.fine", 10)

	require.Eventually(t, func() bool { return logged.counted("re.mgmt.SER-1.term_write.flooded") },
		10*testFailureLogPeriod, 20*time.Millisecond, "the flood was never counted")
}

func TestRegisterLogsRefusalsAndFailuresAFewTimesAPeriod(t *testing.T) {
	lines := logsOfChild(t, floodRegistrations)
	if lines == nil {
		return
	}

	refusals := withMessage(lines, "Refused a call of re.mgmt.SER-1.prune_images")
	assert.Len(t, refusals, 5, "a flood of refusals logs a burst, not every call")
	for _, line := range refusals {
		assert.Equal(t, "warn", line["level"], "a refusal is the check working, not the agent failing")
		assert.NotContains(t, line, "stack", "a refusal needs no stack")
		assert.Equal(t, "777", line["caller_authid"])
		assert.Equal(t, "insufficient privileges to prune images", line["error"])
	}

	failures := withMessage(lines, "An error occured during invocation of re.mgmt.SER-1.prune_images")
	assert.Len(t, failures, 3, "a flood of refusals hides no failure")
	for _, line := range failures {
		assert.Equal(t, "error", line["level"])
		assert.Contains(t, line, "stack")
	}

	assert.Len(t, withMessage(lines, "Refused a call of re.mgmt.SER-1.system_reboot"), 3,
		"a flood of one registration silences no other")

	assert.Equal(t, []float64{195}, counts(lines, "refused calls of re.mgmt.SER-1.prune_images"),
		"the flood is counted once its period is over")
	assertCountsWarn(t, lines, "refused calls of re.mgmt.SER-1.prune_images")
	assert.Empty(t, counts(lines, "failed calls of re.mgmt.SER-1.prune_images"), "nothing of it went unlogged")
	assert.Empty(t, counts(lines, "re.mgmt.SER-1.system_reboot"), "nothing of it went unlogged")
}

// floodRegistrations has 200 calls of one procedure refused, 3 of it fail and
// 3 of another refused.
func floodRegistrations(t *testing.T, logged *childLog) {
	nc := newEventClient()
	session := &WampSession{client: nc, failureLogPeriod: testFailureLogPeriod}

	// Errors with a stack, so a line that logs one shows it.
	prune := func(ctx context.Context, call Result) (*InvokeResult, error) {
		if call.Details["caller_authid"] != "system" {
			return nil, errdefs.InsufficientPrivileges(pkgerrors.New("insufficient privileges to prune images"))
		}
		return nil, pkgerrors.New("prune boom")
	}
	reboot := func(ctx context.Context, call Result) (*InvokeResult, error) {
		return nil, errdefs.InsufficientPrivileges(pkgerrors.New("insufficient privileges to reboot"))
	}
	require.NoError(t, session.Register("re.mgmt.SER-1.prune_images", prune, nil))
	require.NoError(t, session.Register("re.mgmt.SER-1.system_reboot", reboot, nil))

	for i := 0; i < 200; i++ {
		res := nc.invoke(t, "re.mgmt.SER-1.prune_images", "777", nil)
		// Sampling spares the log, never the answer.
		require.Equal(t, wamp.URI("wamp.error.canceled"), res.Err)
		require.Equal(t, wamp.List{wamp.Dict{"error": "insufficient privileges to prune images"}}, res.Args)
	}
	for i := 0; i < 3; i++ {
		res := nc.invoke(t, "re.mgmt.SER-1.prune_images", "system", nil)
		require.Equal(t, wamp.List{wamp.Dict{"error": "prune boom"}}, res.Args)
		nc.invoke(t, "re.mgmt.SER-1.system_reboot", "778", nil)
	}

	require.Eventually(t, func() bool { return logged.counted("refused calls of re.mgmt.SER-1.prune_images") },
		10*testFailureLogPeriod, 20*time.Millisecond, "the flood was never counted")
}

// The count starts over every period: a flood in a later period is counted
// again, and each period logs its burst.
func TestFailureLogCountsTheFloodOfEveryPeriod(t *testing.T) {
	lines := logsOfChild(t, floodTwoPeriods)
	if lines == nil {
		return
	}

	assert.Len(t, withMessage(lines, "An error occured during the subscribe result of re.mgmt.SER-1.term_write.flooded"), 10,
		"each period logs a burst")
	assert.Equal(t, []float64{195, 195}, counts(lines, "failed events of re.mgmt.SER-1.term_write.flooded"),
		"each period's flood is counted")
}

// floodTwoPeriods refuses 200 events in one period, and 200 more once that
// period's count has been logged.
func floodTwoPeriods(t *testing.T, logged *childLog) {
	nc := newEventClient()
	session := &WampSession{client: nc, failureLogPeriod: testFailureLogPeriod}

	refuse := func(Result) error {
		return errors.New("dropped keystrokes from a publisher that does not own the terminal")
	}
	require.NoError(t, session.Subscribe("re.mgmt.SER-1.term_write.flooded", refuse, nil))

	for period := 1; period <= 2; period++ {
		nc.deliver(t, "re.mgmt.SER-1.term_write.flooded", 200)
		require.Eventually(t, func() bool { return logged.countsLogged("re.mgmt.SER-1.term_write.flooded") == period },
			10*testFailureLogPeriod, 20*time.Millisecond, "the flood of period %d was never counted", period)
	}
}

// A recovered panic is logged, with its stack, and sampled like any other
// failure: whoever found the call that panics a handler may repeat it.
func TestRecoveredPanicsAreLoggedAFewTimesAPeriod(t *testing.T) {
	lines := logsOfChild(t, panicHandlers)
	if lines == nil {
		return
	}

	for _, recovery := range []struct {
		message string
		count   string
	}{
		{"Recovered a panic during invocation of re.mgmt.SER-1.get_app_log_history: ", "failed calls of re.mgmt.SER-1.get_app_log_history"},
		{"Recovered a panic during the subscribe result of re.mgmt.SER-1.term_write.panicky: ", "failed events of re.mgmt.SER-1.term_write.panicky"},
	} {
		var recovered []map[string]interface{}
		for _, line := range lines {
			if message, _ := line["message"].(string); strings.HasPrefix(message, recovery.message) {
				recovered = append(recovered, line)
			}
		}

		assert.Len(t, recovered, 5, "%s: a burst, not every panic", recovery.message)
		for _, line := range recovered {
			assert.Equal(t, "error", line["level"])
			assert.Contains(t, line["message"], "index out of range", "the panic")
			assert.Contains(t, line["message"], "runtime/debug.Stack", "its stack")
		}
		assert.Equal(t, []float64{2}, counts(lines, recovery.count), "the rest is counted")
	}
}

// panicHandlers has a registration's handler and a subscription's callback
// panic 7 times each.
func panicHandlers(t *testing.T, logged *childLog) {
	nc := newEventClient()
	session := &WampSession{client: nc, failureLogPeriod: testFailureLogPeriod}

	firstArg := func(ctx context.Context, call Result) (*InvokeResult, error) {
		return &InvokeResult{Arguments: []interface{}{call.Arguments[0]}}, nil
	}
	secondArg := func(event Result) error {
		_ = event.Arguments[1]
		return nil
	}
	require.NoError(t, session.Register("re.mgmt.SER-1.get_app_log_history", firstArg, nil))
	require.NoError(t, session.Subscribe("re.mgmt.SER-1.term_write.panicky", secondArg, nil))

	for i := 0; i < 7; i++ {
		res := nc.invoke(t, "re.mgmt.SER-1.get_app_log_history", "4242", wamp.List{})
		require.Equal(t, wamp.URI("wamp.error.canceled"), res.Err)
	}
	nc.deliver(t, "re.mgmt.SER-1.term_write.panicky", 7)

	require.Eventually(t, func() bool {
		return logged.counted("failed calls of re.mgmt.SER-1.get_app_log_history") &&
			logged.counted("failed events of re.mgmt.SER-1.term_write.panicky")
	}, 10*testFailureLogPeriod, 20*time.Millisecond, "the panics were never counted")
}

// =============================================================================
// A panicking handler costs its call, not the agent
//
// The client runs every invocation handler on a goroutine of its own and every
// event handler on its receive loop, and recovers neither: a handler that
// indexes the arguments of a malformed call would take the whole agent down.
// =============================================================================

func TestRegisterAnswersAPanickingHandlerWithAnError(t *testing.T) {
	nc := newEventClient()
	session := &WampSession{client: nc}

	handled := 0
	firstArg := func(ctx context.Context, call Result) (*InvokeResult, error) {
		handled++
		return &InvokeResult{Arguments: []interface{}{call.Arguments[0]}}, nil
	}
	require.NoError(t, session.Register("re.mgmt.SER-1.get_app_log_history", firstArg, nil))

	res := nc.invoke(t, "re.mgmt.SER-1.get_app_log_history", "4242", wamp.List{})
	assert.Equal(t, wamp.URI("wamp.error.canceled"), res.Err)
	assert.Equal(t, wamp.List{wamp.Dict{"error": errHandlerPanicked.Error()}}, res.Args)

	// Still registered and answering.
	res = nc.invoke(t, "re.mgmt.SER-1.get_app_log_history", "4242", wamp.List{"x"})
	assert.Empty(t, res.Err)
	assert.Equal(t, wamp.List{"x"}, res.Args)
	assert.Equal(t, 2, handled)
}

func TestSubscribeSurvivesAPanickingCallback(t *testing.T) {
	nc := newEventClient()
	session := &WampSession{client: nc}

	var handled atomic.Int32
	firstArg := func(event Result) error {
		handled.Add(1)
		_ = event.Arguments[0]
		return nil
	}
	require.NoError(t, session.Subscribe("wamp.registration.on_unregister", firstArg, nil))

	nc.mu.Lock()
	handler := nc.handlers["wamp.registration.on_unregister"]
	nc.mu.Unlock()

	// On a goroutine of its own, as the client's receive loop: a panic that
	// got out would end the test binary, not just this test.
	for _, args := range []wamp.List{{}, {uint64(1), uint64(2)}} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler(&wamp.Event{Subscription: 1, Arguments: args})
		}()
		<-done
	}

	assert.Equal(t, int32(2), handled.Load())
}

// An empty answer to the status update is no answer: indexing it panicked the
// heartbeat.
func TestUpdateRemoteDeviceStatusToleratesAnEmptyAnswer(t *testing.T) {
	session, err := NewWampSession(testConfig(), &SocketConfig{ConnectionTimeout: time.Millisecond * 100}, nil, NewMockClient().
		SetClientConfigurator(func(c *MockNexusClient) { c.SetCallResult(&wamp.Result{Arguments: wamp.List{}}) }).ConnectNet)
	require.NoError(t, err)
	t.Cleanup(session.Close)

	require.NotPanics(t, func() {
		assert.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))
	})
}

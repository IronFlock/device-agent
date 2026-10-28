package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/testutil/mocks"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// queryAppLogsHandler - windowed container logs
// =============================================================================

func stampedBody(count int) string {
	lines := make([]string, 0, count)
	for i := 0; i < count; i++ {
		lines = append(lines, fmt.Sprintf("2026-08-04T09:%02d:00.000000000Z line-%d", i, i))
	}
	return strings.Join(lines, "\n") + "\n"
}

func expectLogs(cont *mocks.Container, body string) {
	cont.EXPECT().Logs(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, _ common.Dict) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body)), nil
		})
}

func payloadOf(t *testing.T, res *messenger.InvokeResult) common.Dict {
	t.Helper()
	require.NotNil(t, res)
	require.Len(t, res.Arguments, 1)
	payload, ok := res.Arguments[0].(common.Dict)
	require.True(t, ok)
	return payload
}

// newAppLogsEx is a privileged External on a device where prod_1_logapp is
// installed — the only kind of container the log procedures may read.
func newAppLogsEx(t *testing.T) (*External, *mocks.Container) {
	t.Helper()
	ex, cont, db := newLogManagerEx(t, true)
	seedApp(t, db, 1, "logapp", common.PROD)
	return ex, cont
}

func TestQueryAppLogsHandler(t *testing.T) {
	t.Run("returns a windowed slice with its provenance", func(t *testing.T) {
		ex, cont := newAppLogsEx(t)
		expectLogs(cont, stampedBody(10))

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"containerName": "prod_1_logapp",
				"tail":          uint64(3),
			}},
		})

		require.NoError(t, err)
		payload := payloadOf(t, res)

		assert.Equal(t, "docker", payload["source"])
		assert.Equal(t, 3, payload["returned"])
		assert.Equal(t, true, payload["truncated"])
		assert.NotEmpty(t, payload["device_time"])
		assert.Equal(t, "2026-08-04T09:00:00Z", payload["oldest_available"])

		lines, ok := payload["lines"].([]string)
		require.True(t, ok)
		assert.Contains(t, lines[2], "line-9")
	})

	// nexus hands WAMP integers over as uint64, but the handler is also driven
	// from Go and from serializers that disagree; all of them mean 3 lines.
	t.Run("accepts any integral shape for tail and offset", func(t *testing.T) {
		for name, value := range map[string]interface{}{
			"uint64":  uint64(3),
			"int":     3,
			"float64": float64(3),
			"string":  "3",
		} {
			ex, cont := newAppLogsEx(t)
			expectLogs(cont, stampedBody(10))

			res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
				Details: systemDetails(),
				Arguments: []interface{}{map[string]interface{}{
					"containerName": "prod_1_logapp",
					"tail":          value,
				}},
			})

			require.NoError(t, err, name)
			assert.Equal(t, 3, payloadOf(t, res)["returned"], name)
		}
	})

	t.Run("accepts a relative window", func(t *testing.T) {
		ex, cont := newAppLogsEx(t)
		expectLogs(cont, stampedBody(4))

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"containerName": "prod_1_logapp",
				"since":         "30m",
			}},
		})

		require.NoError(t, err)
		assert.NotNil(t, payloadOf(t, res)["lines"])
	})

	t.Run("rejects a window it cannot date", func(t *testing.T) {
		ex, _, _ := newLogManagerEx(t, true)

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"containerName": "prod_1_logapp",
				"since":         "yesterday",
			}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "since")
	})

	t.Run("rejects a tail that is not a count", func(t *testing.T) {
		ex, _, _ := newLogManagerEx(t, true)

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"containerName": "prod_1_logapp",
				"tail":          -5,
			}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects a missing or malformed argument list", func(t *testing.T) {
		for name, args := range map[string][]interface{}{
			"nil":          nil,
			"empty":        {},
			"nil first":    {nil},
			"not a dict":   {"nope"},
			"no container": {map[string]interface{}{}},
		} {
			ex, _, _ := newLogManagerEx(t, true)

			res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: args,
			})

			require.Error(t, err, name)
			assert.Nil(t, res, name)
		}
	})

	// On an appliance the agent's host also runs the platform. A READ privilege
	// on the device reaches its apps' logs, never the platform's.
	t.Run("answers a container that is no app here as not found, unread", func(t *testing.T) {
		ex, _ := newAppLogsEx(t)

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"containerName": "ironflock-postgres-1"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsContainerNotFound(err))
	})

	t.Run("denies an unprivileged caller before reading anything", func(t *testing.T) {
		details, m := grantPrivilege(false)
		cont := mocks.NewContainer(t)
		ex := &External{LogManager: nil, Container: cont, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.queryAppLogsHandler(context.Background(), messenger.Result{
			Details:   details,
			Arguments: []interface{}{map[string]interface{}{"containerName": "prod_1_logapp"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// queryDeviceLogsHandler - windowed, severity-filtered agent log
// =============================================================================

// newDeviceLogEx wires an External whose config points at a temp reagent.log.
func newDeviceLogEx(t *testing.T, granted bool, lines ...string) *External {
	t.Helper()

	cfg := testConfig()
	logFile := filepath.Join(t.TempDir(), "reagent.log")
	require.NoError(t, os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	cfg.CommandLineArguments.LogFileLocation = logFile

	return &External{Config: cfg, Privilege: priv(t, granted)}
}

func agentEntry(stamp, level, message string) string {
	return fmt.Sprintf(`{"level":"%s","time":"%s","message":"%s"}`, level, stamp, message)
}

func TestQueryDeviceLogsHandler(t *testing.T) {
	// "What has this device been doing?" is a complete question. Forcing an
	// argument dict onto it would be ceremony.
	t.Run("answers with no arguments at all", func(t *testing.T) {
		ex := newDeviceLogEx(t, true,
			agentEntry("2026-08-04T09:00:00Z", "info", "starting app"))

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: nil,
		})

		require.NoError(t, err)
		payload := payloadOf(t, res)
		assert.Equal(t, 1, payload["returned"])
		assert.Equal(t, 1, payload["files_scanned"])
		assert.Equal(t, "2026-08-04T09:00:00Z", payload["oldest_available"])
	})

	t.Run("filters by severity", func(t *testing.T) {
		ex := newDeviceLogEx(t, true,
			agentEntry("2026-08-04T09:00:00Z", "info", "routine"),
			agentEntry("2026-08-04T09:01:00Z", "error", "failed to pull image"))

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"level": "error"}},
		})

		require.NoError(t, err)
		payload := payloadOf(t, res)
		require.Equal(t, 1, payload["returned"])

		lines, ok := payload["lines"].([]string)
		require.True(t, ok)
		assert.Contains(t, lines[0], "failed to pull image")
	})

	t.Run("rejects a non-string level", func(t *testing.T) {
		ex := newDeviceLogEx(t, true, agentEntry("2026-08-04T09:00:00Z", "info", "x"))

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"level": 3}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects a non-dict argument", func(t *testing.T) {
		ex := newDeviceLogEx(t, true, agentEntry("2026-08-04T09:00:00Z", "info", "x"))

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{"nope"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	// Unlike its predecessor, this reads the device's own log only for a caller
	// that holds READ on the device.
	t.Run("denies an unprivileged caller", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   details,
			Arguments: nil,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})

	// A privilege check that could not reach the cloud is NOT a denial. Saying
	// "insufficient privileges" during a cloud incident sends an operator
	// chasing permissions in exactly the moment they need the device log.
	t.Run("a privilege lookup failure is not reported as a denial", func(t *testing.T) {
		details, m := grantPrivilege(true)
		m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{}, errors.New("no route to the router"))
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.queryDeviceLogsHandler(context.Background(), messenger.Result{
			Details:   details,
			Arguments: nil,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
	})
}

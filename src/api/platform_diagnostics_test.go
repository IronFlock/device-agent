package api

import (
	"context"
	"fmt"
	"io"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"reagent/logging"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var platformBackend = common.Dict{"caller_authid": "system", "caller_authrole": "system"}

func platformEx(t *testing.T, granted bool) (*External, *mocks.Container, *fakes.Messenger) {
	t.Helper()

	ex, cont, _ := newLogManagerEx(t, granted)
	ex.Config = testConfig()

	m := fakes.NewMessenger()
	m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
		Arguments: []interface{}{granted},
	}, nil)
	ex.Privilege = newPrivilege(testConfig(), m)

	return ex, cont, m
}

func onePlatformContainer(cont *mocks.Container) {
	cont.EXPECT().ListContainers(mock.Anything, mock.Anything).Return([]container.ContainerResult{{
		ID:    "id-backend",
		Names: []string{"/ironflock-appliance-backend-1"},
		State: "running",
		Labels: map[string]string{
			container.ComposeProjectLabel: logging.PlatformComposeProject,
			container.ComposeServiceLabel: "backend",
		},
	}}, nil)
}

func TestPlatformProceduresAreBackendOnly(t *testing.T) {
	for _, topic := range []topics.Topic{topics.ListPlatformServices, topics.QueryPlatformLogs} {
		t.Run(string(topic), func(t *testing.T) {
			// The container mock is strict: reaching Docker would fail the test.
			ex, _, _ := platformEx(t, true)

			_, err := ex.getTopicHandlerMap()[topic](context.Background(), messenger.Result{
				Details:   common.Dict{"caller_authid": "42", "caller_authrole": "acct_manager"},
				Arguments: []interface{}{map[string]interface{}{"source": "backend"}},
			})

			assert.True(t, errdefs.IsInsufficientPrivileges(err), "got %v", err)
		})
	}
}

func TestPlatformProceduresCheckMaintainAsTheNamedAccount(t *testing.T) {
	for _, topic := range []topics.Topic{topics.ListPlatformServices, topics.QueryPlatformLogs} {
		t.Run(string(topic), func(t *testing.T) {
			ex, _, m := platformEx(t, false)

			details := common.Dict{"caller_authid": "999", "caller_authrole": "system"}
			_, err := ex.getTopicHandlerMap()[topic](context.Background(), messenger.Result{
				Details:   details,
				Arguments: []interface{}{map[string]interface{}{"source": "backend"}},
			})

			assert.True(t, errdefs.IsInsufficientPrivileges(err), "got %v", err)

			calls := m.GetCallCalls()
			require.Len(t, calls, 1)
			payload, ok := calls[0].Args[0].(common.Dict)
			require.True(t, ok)
			assert.Equal(t, "MAINTAIN", payload["privilege"])
			assert.Equal(t, 999, payload["requestor_account_key"])
		})
	}
}

func TestQueryPlatformLogsHandlerAnswers(t *testing.T) {
	ex, cont, _ := platformEx(t, true)
	onePlatformContainer(cont)

	var options []common.Dict
	body := fmt.Sprintf("%s fatal\n%s ok\n", "2026-08-04T09:00:00.000000000Z", "2026-08-04T09:01:00.000000000Z")
	cont.EXPECT().Logs(mock.Anything, "id-backend", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, o common.Dict) (io.ReadCloser, error) {
			options = append(options, o)
			return io.NopCloser(strings.NewReader(body)), nil
		})

	res, err := ex.queryPlatformLogsHandler(context.Background(), messenger.Result{
		Details: platformBackend,
		Arguments: []interface{}{map[string]interface{}{
			"source": "backend", "tail": uint64(10), "since": "2026-08-04T08:00:00Z",
		}},
	})

	require.NoError(t, err)
	payload := payloadOf(t, res)
	assert.Equal(t, 2, payload["returned"])
	assert.Equal(t, "compose", payload["source"])
	assert.Equal(t, "2026-08-04T09:00:00Z", payload["oldest_available"])
	assert.Equal(t, "10", options[0]["tail"])
	assert.Equal(t, "2026-08-04T08:00:00Z", options[0]["since"])
}

func TestQueryPlatformLogsHandlerRejectsBadArguments(t *testing.T) {
	for name, arguments := range map[string]map[string]interface{}{
		"no source":        {},
		"empty source":     {"source": ""},
		"source not text":  {"source": 7},
		"search not text":  {"source": "backend", "search": 7},
		"tail not a count": {"source": "backend", "tail": "many"},
		"bad since":        {"source": "backend", "since": "yesterdayish"},
	} {
		t.Run(name, func(t *testing.T) {
			ex, cont, _ := platformEx(t, true)
			if _, hasSource := arguments["source"].(string); hasSource && arguments["source"] != "" {
				// Only reached when parsing wrongly lets the call through.
				cont.EXPECT().ListContainers(mock.Anything, mock.Anything).Maybe().Return(nil, nil)
			}

			_, err := ex.queryPlatformLogsHandler(context.Background(), messenger.Result{
				Details:   platformBackend,
				Arguments: []interface{}{arguments},
			})

			require.Error(t, err)
			assert.NotErrorIs(t, err, logging.ErrNotAppliance, "must fail on the arguments, before the read")
		})
	}
}

func TestListPlatformServicesHandlerAnswers(t *testing.T) {
	ex, cont, _ := platformEx(t, true)
	onePlatformContainer(cont)
	cont.EXPECT().GetContainerState(mock.Anything, "id-backend").Return(container.ContainerState{
		Health: "healthy",
	}, nil)

	res, err := ex.listPlatformServicesHandler(context.Background(), messenger.Result{Details: platformBackend})

	require.NoError(t, err)
	payload := payloadOf(t, res)

	services, ok := payload["services"].([]common.Dict)
	require.True(t, ok)
	require.Len(t, services, 1)
	assert.Equal(t, "healthy", services[0]["health"])

	host, ok := payload["host"].(common.Dict)
	require.True(t, ok)
	assert.Contains(t, host, "storage_free")
	assert.Contains(t, host, "memory_available")
	assert.NotEmpty(t, payload["device_time"])
}

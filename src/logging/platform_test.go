package logging

import (
	"context"
	"io"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"reagent/testutil/mocks"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func platformContainer(id, service string) container.ContainerResult {
	return container.ContainerResult{
		ID:    id,
		Names: []string{"/ironflock-appliance-" + service + "-1"},
		Image: "instance-registry.ironflock.com/" + service + ":v1",
		State: "running",
		Labels: map[string]string{
			container.ComposeProjectLabel: PlatformComposeProject,
			container.ComposeServiceLabel: service,
		},
	}
}

func expectPlatform(cont *mocks.Container, containers ...container.ContainerResult) {
	cont.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(containers, nil)
}

// expectBody answers every read of the container with the same stamped body and
// records what the read asked for.
func expectBody(cont *mocks.Container, id, body string, reads *[]common.Dict) {
	cont.EXPECT().Logs(mock.Anything, id, mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, options common.Dict) (io.ReadCloser, error) {
			if reads != nil {
				*reads = append(*reads, options)
			}
			return reader(body), nil
		})
}

func stubJournal(t *testing.T, output string) *[][]string {
	t.Helper()

	var calls [][]string
	previous := runJournalctl
	runJournalctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(output), nil
	}
	t.Cleanup(func() { runJournalctl = previous })

	return &calls
}

func TestPlatformReadsRefuseAnOrdinaryDevice(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont)
	calls := stubJournal(t, "2026-08-04T09:00:00+0000 host secret\n")

	_, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: PlatformSourceHost})
	assert.ErrorIs(t, err, ErrNotAppliance)
	assert.Empty(t, *calls, "the journal must not be read on a device without the stack")

	_, err = lm.PlatformServices(context.Background())
	assert.ErrorIs(t, err, ErrNotAppliance)
}

func TestQueryPlatformLogsReadsOnlyTheNamedService(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"), platformContainer("id-kratos", "kratos"))
	expectBody(cont, "id-backend", stampedLines(2), nil)

	result, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: "backend"})

	require.NoError(t, err)
	require.Len(t, result.Lines, 2)
	assert.Contains(t, result.Lines[0], "backend-1  | ")
	assert.Contains(t, result.Lines[1], "line-1")
	assert.Equal(t, sourceCompose, result.Source)
}

func TestQueryPlatformLogsRefusesAnUnknownService(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))

	_, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: "prod_1_someapp"})

	assert.True(t, errdefs.IsContainerNotFound(err), "got %v", err)
}

func TestQueryPlatformLogsForwardsTheWindow(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	var reads []common.Dict
	expectBody(cont, "id-backend", stampedLines(1), &reads)

	since := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)

	_, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{
		Source: "backend", Tail: 50, Since: since, Until: until,
	})

	require.NoError(t, err)
	require.NotEmpty(t, reads)
	assert.Equal(t, "50", reads[0]["tail"])
	assert.Equal(t, "2026-08-04T09:00:00Z", reads[0]["since"])
	assert.Equal(t, "2026-08-04T10:00:00Z", reads[0]["until"])
}

func TestQueryPlatformLogsSearchesBeforeTheTail(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	var reads []common.Dict

	body := strings.Join([]string{
		"2026-08-04T09:00:00.000000000Z FATAL could not connect",
		"2026-08-04T09:01:00.000000000Z fine",
		"2026-08-04T09:02:00.000000000Z fine again",
	}, "\n") + "\n"
	expectBody(cont, "id-backend", body, &reads)

	result, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{
		Source: "backend", Tail: 1, Search: "fatal",
	})

	require.NoError(t, err)
	require.Len(t, result.Lines, 1)
	assert.Contains(t, result.Lines[0], "FATAL could not connect")
	assert.Equal(t, "5000", reads[0]["tail"], "a search has to scan past the tail")
}

func TestQueryPlatformLogsCapsTheTail(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	var reads []common.Dict
	expectBody(cont, "id-backend", stampedLines(1), &reads)

	_, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: "backend", Tail: 999999})

	require.NoError(t, err)
	assert.Equal(t, "5000", reads[0]["tail"])
}

func TestHostJournalReadsTheFixedUnits(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	calls := stubJournal(t, "2026-08-04T09:00:00+0000 host systemd[1]: Started ironflock.service\n")

	result, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: PlatformSourceHost, Tail: 10})

	require.NoError(t, err)
	assert.Equal(t, sourceJournal, result.Source)
	require.Len(t, result.Lines, 1)
	require.Len(t, *calls, 1)

	args := strings.Join((*calls)[0], " ")
	assert.Contains(t, args, "-u ironflock*.service")
	assert.Contains(t, args, "-u docker.service")
	assert.Contains(t, args, "-n 10")
	assert.NotContains(t, args, "_TRANSPORT=kernel")
}

func TestKernelJournalCoversEarlierBoots(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	calls := stubJournal(t, "")

	result, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{Source: PlatformSourceKernel})

	require.NoError(t, err)
	assert.Empty(t, result.Lines)

	args := (*calls)[0]
	assert.Contains(t, args, "_TRANSPORT=kernel")
	assert.NotContains(t, args, "-k", "-k would limit the read to the current boot")
	assert.NotContains(t, args, "-b")
}

func TestJournalWindowAndSearchNeverReachTheArguments(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-backend", "backend"))
	calls := stubJournal(t, "2026-08-04T09:00:00+0000 host a match\n2026-08-04T09:01:00+0000 host other\n")

	since := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)

	result, err := lm.QueryPlatformLogs(context.Background(), PlatformLogRequest{
		Source: PlatformSourceHost, Since: since, Search: "--output=json; MATCH",
	})

	require.NoError(t, err)
	assert.Empty(t, result.Lines, "the search is a plain substring")

	args := (*calls)[0]
	assert.Contains(t, args, journalTime(since))
	for _, arg := range args {
		assert.NotContains(t, arg, "output=json")
	}
}

func TestPlatformServicesReportsStateSorted(t *testing.T) {
	lm, cont, _, _ := newTestManager(t)
	expectPlatform(cont, platformContainer("id-router", "router"), platformContainer("id-backend", "backend"))

	cont.EXPECT().GetContainerState(mock.Anything, "id-backend").Return(container.ContainerState{
		Health: "unhealthy", RestartCount: 3, OOMKilled: true, ExitCode: 137, Error: "boom",
	}, nil)
	cont.EXPECT().GetContainerState(mock.Anything, "id-router").Return(container.ContainerState{}, errdefs.ContainerNotFound(assert.AnError))

	services, err := lm.PlatformServices(context.Background())

	require.NoError(t, err)
	require.Len(t, services, 2)

	backend := services[0]
	assert.Equal(t, "backend", backend["service"])
	assert.Equal(t, "instance-registry.ironflock.com/backend:v1", backend["image"])
	assert.Equal(t, "unhealthy", backend["health"])
	assert.Equal(t, 3, backend["restart_count"])
	assert.Equal(t, true, backend["oom_killed"])
	assert.Equal(t, 137, backend["exit_code"])
	assert.Equal(t, "boom", backend["error"])

	router := services[1]
	assert.Equal(t, "router", router["service"])
	assert.Equal(t, "running", router["state"], "the listing's answer survives a failed inspect")
	assert.NotContains(t, router, "restart_count")
}

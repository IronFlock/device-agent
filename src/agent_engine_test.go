package main

import (
	"context"
	"reagent/api"
	"reagent/common"
	"reagent/container"
	"reagent/messenger/topics"
	"reagent/safe"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The connect's report goes out at once while the Docker daemon hangs, without
// the engine's keys.
func TestConnectReportDoesNotWaitForDocker(t *testing.T) {
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	reading := make(chan struct{}, 1)
	engine := api.NewEngineReport(func(ctx context.Context) (container.EngineInfo, error) {
		reading <- struct{}{}
		<-hung // longer than any timeout
		return container.EngineInfo{}, context.DeadlineExceeded
	})
	safe.Go(engine.Refresh)
	<-reading

	m := fakes.NewMessenger()
	agent := &Agent{Messenger: m, External: &api.External{Config: builders.DefaultTestConfig(), Messenger: m, Engine: engine}}

	start := time.Now()
	require.NoError(t, agent.updateRemoteDevice())
	assert.Less(t, time.Since(start), time.Second)

	require.Equal(t, 1, m.GetCallCount())
	call := m.GetCallCalls()[0]
	assert.Equal(t, topics.UpdateDeviceArchitecture, call.Topic)
	report := call.Args[0].(common.Dict)
	assert.Contains(t, report, "architecture")
	assert.NotContains(t, report, "engine_platform")
	assert.NotContains(t, report, "engine_version")
}

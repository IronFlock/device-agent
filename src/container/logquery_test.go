package container

import (
	"reagent/common"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The renderer below is the whole contract between a windowed log query and the
// Docker API that serves it. Getting an option name or a zero-value default
// wrong here is invisible at compile time and shows up as "the time range was
// ignored", so it is asserted directly rather than through a daemon.

func TestLogQueryDockerOptions(t *testing.T) {
	t.Run("zero value reads everything without timestamps", func(t *testing.T) {
		options := LogQuery{}.DockerOptions()

		assert.Equal(t, "all", options["tail"])
		assert.Equal(t, false, options["follow"])
		assert.Equal(t, true, options["stdout"])
		assert.Equal(t, true, options["stderr"])
		assert.NotContains(t, options, "since")
		assert.NotContains(t, options, "until")
		assert.NotContains(t, options, "timestamps")
	})

	t.Run("carries every bound it was given", func(t *testing.T) {
		options := LogQuery{
			Tail:       200,
			Since:      "2026-08-04T09:00:00Z",
			Until:      "2026-08-04T09:15:00Z",
			Timestamps: true,
		}.DockerOptions()

		assert.Equal(t, "200", options["tail"])
		assert.Equal(t, "2026-08-04T09:00:00Z", options["since"])
		assert.Equal(t, "2026-08-04T09:15:00Z", options["until"])
		assert.Equal(t, true, options["timestamps"])
	})
}

func TestLogsOptionsFromDict(t *testing.T) {
	t.Run("maps every recognised key", func(t *testing.T) {
		options := logsOptionsFromDict(common.Dict{
			"stdout":     true,
			"stderr":     true,
			"follow":     false,
			"tail":       "120",
			"since":      "2026-08-04T09:00:00Z",
			"until":      "2026-08-04T09:15:00Z",
			"timestamps": true,
		})

		assert.True(t, options.ShowStdout)
		assert.True(t, options.ShowStderr)
		assert.False(t, options.Follow)
		assert.Equal(t, "120", options.Tail)
		assert.Equal(t, "2026-08-04T09:00:00Z", options.Since)
		assert.Equal(t, "2026-08-04T09:15:00Z", options.Until)
		assert.True(t, options.Timestamps)
	})

	// The regression this guards: nexus decodes a WAMP integer as uint64, the
	// old code asserted only string, and a caller asking for 100 lines silently
	// got the entire log because Tail stayed empty and Docker defaults to "all".
	t.Run("accepts a numeric tail", func(t *testing.T) {
		assert.Equal(t, "100", logsOptionsFromDict(common.Dict{"tail": uint64(100)}).Tail)
		assert.Equal(t, "100", logsOptionsFromDict(common.Dict{"tail": 100}).Tail)
		assert.Equal(t, "100", logsOptionsFromDict(common.Dict{"tail": float64(100)}).Tail)
	})

	t.Run("ignores an unusable value rather than failing the read", func(t *testing.T) {
		options := logsOptionsFromDict(common.Dict{"tail": []string{"nope"}, "since": 42})

		assert.Empty(t, options.Tail)
		assert.Empty(t, options.Since)
	})
}

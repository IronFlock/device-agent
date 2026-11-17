package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reagent/config"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetupLoggerWritesTheStartLineAtInfo: ironflock-cli's tests take
// "REAgent CLI Arguments" as the start of the agent's last run in its log,
// so the line is there at the level info as well as at debug.
func TestSetupLoggerWritesTheStartLineAtInfo(t *testing.T) {
	prevLogger, prevLevel := log.Logger, zerolog.GlobalLevel()
	t.Cleanup(func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	})

	for name, tc := range map[string]struct {
		debug bool
		level zerolog.Level
	}{
		"-debug":       {debug: true, level: zerolog.DebugLevel},
		"-debug=false": {debug: false, level: zerolog.InfoLevel},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "device-agent.log")
			SetupLogger(&config.CommandLineArguments{LogFileLocation: path, Debug: tc.debug})
			assert.Equal(t, tc.level, zerolog.GlobalLevel())

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			require.Len(t, lines, 1, string(data))
			var line struct {
				Level   string `json:"level"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &line))
			assert.Equal(t, "info", line.Level)
			assert.True(t, strings.HasPrefix(line.Message, "REAgent CLI Arguments: "), line.Message)
		})
	}
}

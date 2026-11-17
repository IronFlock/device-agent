package messenger

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConnectionIsLoggedAtInfo: ironflock-cli's tests count "Successfully
// established a connection" in the agent's log to see it connect, so the
// line is there at the level info, with this text.
func TestConnectionIsLoggedAtInfo(t *testing.T) {
	lines := logsOfChild(t, func(t *testing.T, _ *childLog) {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)

		session, err := NewWampSession(testConfig(), &SocketConfig{ConnectionTimeout: time.Millisecond * 100}, nil, NewMockClient().ConnectNet)
		require.NoError(t, err)
		session.Close()
	})
	// Not lines == nil: a child that logs nothing at info gives the parent
	// no lines either, and that is the failure this test is for.
	if os.Getenv(failureLogChild) == "1" {
		return
	}

	var connected []map[string]interface{}
	for _, line := range lines {
		if message, _ := line["message"].(string); strings.HasPrefix(message, "Successfully established a connection") {
			connected = append(connected, line)
		}
	}
	require.Len(t, connected, 1, "%v", lines)
	assert.Equal(t, "info", connected[0]["level"])
}

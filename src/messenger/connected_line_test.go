package messenger

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a log the session's goroutines can write while the test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConnectionIsLoggedAtInfo: ironflock-cli's tests count "Successfully
// established a connection" in the agent's log to see it connect, so the
// line is there at the level info, with this text.
func TestConnectionIsLoggedAtInfo(t *testing.T) {
	logged := &lockedBuffer{}
	prevLogger, prevLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(logged)
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	t.Cleanup(func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	})

	session, err := NewWampSession(testConfig(), &SocketConfig{ConnectionTimeout: time.Millisecond * 100}, nil, NewMockClient().ConnectNet)
	require.NoError(t, err)
	defer session.Close()

	var connected []map[string]interface{}
	for _, line := range decodeLogLines(logged.String()) {
		if message, _ := line["message"].(string); strings.HasPrefix(message, "Successfully established a connection") {
			connected = append(connected, line)
		}
	}
	require.Len(t, connected, 1, logged.String())
	assert.Equal(t, "info", connected[0]["level"])
}

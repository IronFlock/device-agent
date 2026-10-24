//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"reagent/migrate/winmigrate"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
)

// The fleet switch for Windows: only {"windows": true} starts the move; an
// absent key, false or no file keeps the fleet where it is; any other
// answer is an error and nothing starts on a guess.
func TestMigrationEnabled(t *testing.T) {
	cases := map[string]struct {
		status  int
		body    string
		enabled bool
		fails   bool
	}{
		"on":              {http.StatusOK, `{"enabled":true,"windows":true}`, true, false},
		"off":             {http.StatusOK, `{"enabled":true,"windows":false}`, false, false},
		"absent":          {http.StatusOK, `{"enabled":true}`, false, false},
		"empty":           {http.StatusOK, `{}`, false, false},
		"no file":         {http.StatusNotFound, "", false, false},
		"forbidden":       {http.StatusForbidden, "", false, true},
		"server error":    {http.StatusInternalServerError, "", false, true},
		"not json":        {http.StatusOK, `windows: true`, false, true},
		"key of a string": {http.StatusOK, `{"windows":"yes"}`, false, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/"+migrationSwitch, r.URL.Path)
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer server.Close()
			enabled, err := migrationEnabled(server.URL + "/")
			if c.fails {
				require.Error(t, err)
				assert.False(t, enabled)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.enabled, enabled)
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		enabled, err := migrationEnabled(url)
		require.Error(t, err)
		assert.False(t, enabled)
	})
}

// The migrate service's handler reports the service's states around the
// migration's run and maps its exit code: a non-zero code is a
// service-specific error (the recovery actions start the service again),
// zero a clean stop; a stop request closes the engine's stop channel.
func TestMigrateServiceExecuteReportsStates(t *testing.T) {
	drain := func(changes <-chan svc.Status, n int) []svc.Status {
		var seen []svc.Status
		for i := 0; i < n; i++ {
			select {
			case s := <-changes:
				seen = append(seen, s)
			case <-time.After(5 * time.Second):
				t.Fatalf("only %d of %d states reported: %v", len(seen), n, seen)
			}
		}
		return seen
	}
	t.Run("a result that asks for a restart", func(t *testing.T) {
		s := &migrateService{env: &winmigrate.Env{}, serve: func(winmigrate.Env, winmigrate.ServeOptions) uint32 { return 1 }}
		changes := make(chan svc.Status, 8)
		specific, code := s.Execute(nil, make(chan svc.ChangeRequest), changes)
		assert.True(t, specific)
		assert.Equal(t, uint32(1), code)
		states := drain(changes, 3)
		assert.Equal(t, svc.StartPending, states[0].State)
		assert.Equal(t, svc.Running, states[1].State)
		assert.Equal(t, svc.AcceptStop|svc.AcceptShutdown, states[1].Accepts)
		assert.Equal(t, svc.StopPending, states[2].State)
	})
	t.Run("a clean result", func(t *testing.T) {
		s := &migrateService{env: &winmigrate.Env{}, serve: func(winmigrate.Env, winmigrate.ServeOptions) uint32 { return 0 }}
		changes := make(chan svc.Status, 8)
		specific, code := s.Execute(nil, make(chan svc.ChangeRequest), changes)
		assert.False(t, specific)
		assert.Equal(t, uint32(0), code)
		drain(changes, 3)
	})
	t.Run("a stop request", func(t *testing.T) {
		stopped := make(chan struct{})
		s := &migrateService{env: &winmigrate.Env{}, serve: func(_ winmigrate.Env, opts winmigrate.ServeOptions) uint32 {
			<-opts.Stop
			close(stopped)
			return 0
		}}
		changes := make(chan svc.Status, 8)
		requests := make(chan svc.ChangeRequest)
		result := make(chan uint32, 1)
		go func() { _, code := s.Execute(nil, requests, changes); result <- code }()
		states := drain(changes, 2)
		require.Equal(t, svc.Running, states[1].State)
		requests <- svc.ChangeRequest{Cmd: svc.Stop}
		pending := drain(changes, 1)[0]
		assert.Equal(t, svc.StopPending, pending.State)
		assert.Equal(t, uint32(winmigrate.DefaultStopGrace/time.Millisecond), pending.WaitHint)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("the engine's stop channel was not closed")
		}
		assert.Equal(t, uint32(0), <-result)
	})
}

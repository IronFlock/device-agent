package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/filesystem"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Every agent start writes admin credentials of its own into frpc.yaml, and a
// reset keeps them: the frpc that is still running knows only the old ones.
func TestAdminAPICredentials(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	user, password := builder.GetAdminAuth()
	assert.Equal(t, adminUser, user)
	assert.GreaterOrEqual(t, len(password), 26, "rand.Text carries at least 128 bits")

	raw, err := os.ReadFile(builder.ConfigPath)
	require.NoError(t, err)
	var onDisk FrpcYamlConfig
	require.NoError(t, yaml.Unmarshal(raw, &onDisk))
	require.NotNil(t, onDisk.WebServer)
	assert.Equal(t, user, onDisk.WebServer.User)
	assert.Equal(t, password, onDisk.WebServer.Password, "frpc and `frpc reload -c` read the credentials from the file")

	other := NewTunnelConfigBuilder(builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)}))
	_, otherPassword := other.GetAdminAuth()
	assert.NotEqual(t, password, otherPassword, "every start gets a new password")

	builder.Reset()
	_, afterReset := builder.GetAdminAuth()
	assert.Equal(t, password, afterReset, "a reset must keep the running frpc's credentials")
	raw, err = os.ReadFile(builder.ConfigPath)
	require.NoError(t, err)
	assert.Contains(t, string(raw), password, "the kept credentials must reach the file the reload reads")
}

// The status poll authenticates the way frpc expects; without the credentials
// it gets the 401 frpc answers.
func TestAllStatusSendsAdminCredentials(t *testing.T) {
	m := newCapabilityTestManager(t)
	user, password := m.configBuilder.GetAdminAuth()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPassword != password {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"http":[{"name":"t1-http","type":"http","status":"running"}]}`))
	}))
	defer server.Close()

	m.configBuilder.SetCommonVariable(ADMIN_PORT, strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	m.setCapability(CapabilityAvailable, nil)

	statuses, err := m.AllStatus()
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "running", statuses[0].Status)
}

// TestFrpcAdminAPIRequiresCredentials runs the real embedded frpc with the
// config the agent writes (only pointed at a closed local port, so it never
// logs in) and checks every admin route: without the credentials, or with a
// wrong password, frpc answers 401; the agent's own status poll and reload
// still work.
func TestFrpcAdminAPIRequiresCredentials(t *testing.T) {
	frpcBinary, err := filepath.Abs(filepath.Join("..", "embedded", "frpc_binary"))
	require.NoError(t, err)
	if _, err := exec.Command(frpcBinary, "-v").Output(); err != nil {
		t.Skipf("no runnable embedded frpc at %s: %v", frpcBinary, err)
	}
	clearProxyEnv(t)

	m := newCapabilityTestManager(t)
	binary, err := os.ReadFile(frpcBinary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filesystem.GetTunnelBinaryPath(m.config, "frpc"), binary, 0o755))

	// A port nobody listens on: frpc retries its login and keeps the admin
	// API up meanwhile.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverPort := closed.Addr().(*net.TCPAddr).Port
	require.NoError(t, closed.Close())
	m.configBuilder.SetCommonVariable(SERVER_ADDRESS, "127.0.0.1")
	m.configBuilder.SetCommonVariable(SERVER_PORT, strconv.Itoa(serverPort))
	m.configBuilder.SetCommonVariable("log_file", "console")
	m.configBuilder.SaveConfig()

	adminPort, err := m.configBuilder.GetAdminPort()
	require.NoError(t, err)
	user, password := m.configBuilder.GetAdminAuth()
	base := fmt.Sprintf("http://127.0.0.1:%d", adminPort)

	ctx, cancel := context.WithCancel(context.Background())
	output := &syncBuffer{}
	cmd := exec.CommandContext(ctx, frpcBinary, "-c", m.configBuilder.ConfigPath)
	cmd.Env = frpcEnv()
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// /healthz is the one route frpc serves without authentication.
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 100*time.Millisecond, "frpc admin API did not come up; frpc output:\n%s", output)

	request := func(method, path, user, password string) int {
		t.Helper()
		var body *strings.Reader
		if method == http.MethodPut {
			body = strings.NewReader("serverAddr = \"attacker.example\"\n")
		} else {
			body = strings.NewReader("")
		}
		req, err := http.NewRequest(method, base+path, body)
		require.NoError(t, err)
		if user != "" || password != "" {
			req.SetBasicAuth(user, password)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/config"},
		{http.MethodPut, "/api/config"},
		{http.MethodGet, "/api/reload"},
		{http.MethodPost, "/api/stop"},
	}
	for _, route := range routes {
		assert.Equal(t, http.StatusUnauthorized, request(route.method, route.path, "", ""),
			"%s %s without credentials", route.method, route.path)
		assert.Equal(t, http.StatusUnauthorized, request(route.method, route.path, user, password+"x"),
			"%s %s with a wrong password", route.method, route.path)
	}
	assert.Equal(t, http.StatusOK, request(http.MethodGet, "/api/status", user, password))

	m.setCapability(CapabilityAvailable, nil)
	_, err = m.AllStatus()
	require.NoError(t, err, "the agent's status poll must authenticate")
	require.NoError(t, m.Reload(), "`frpc reload -c` must take the credentials from the config; frpc output:\n%s", output)
}

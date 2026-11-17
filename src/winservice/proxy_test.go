package winservice

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseProxyEnvIgnoresCommentsAndTakesQuotesOff(t *testing.T) {
	s := ParseProxyEnv([]byte("# comment\r\nHTTP_PROXY='http://u:@p:1'\r\n https_proxy = \"http://q:2\"\nNO_PROXY=localhost\nOTHER=x\nnot a pair\n"))
	assert.Equal(t, ProxySettings{HTTP: "http://u:@p:1", HTTPS: "http://q:2", NoProxy: "localhost"}, s)
	assert.True(t, s.Configured())
	assert.False(t, ProxySettings{NoProxy: "localhost"}.Configured())
}

func TestRenderProxyEnvRoundTripsSeparateProtocols(t *testing.T) {
	s := ProxySettings{HTTP: "http://user:p%40ss@proxy:3128", HTTPS: "https://other:443", NoProxy: "localhost,127.0.0.1,.corp.example"}
	assert.Equal(t, s, ParseProxyEnv([]byte(RenderProxyEnv(s))))
	assert.Equal(t, ProxySettings{}, ParseProxyEnv([]byte(RenderProxyEnv(ProxySettings{}))))
}

func TestProxyFromEnvironment(t *testing.T) {
	s, found := ProxyFromEnvironment([]string{"DOCKER_HOST=tcp://127.0.0.1:2376", "http_proxy=http://lower:1", "HTTP_PROXY=http://upper:1", "HTTPS_PROXY=", "NO_PROXY=localhost"})
	assert.True(t, found)
	assert.Equal(t, ProxySettings{HTTP: "http://upper:1", NoProxy: "localhost"}, s)

	// Only empty variables, as an older install wrote for a device without
	// a proxy: found, not configured.
	s, found = ProxyFromEnvironment([]string{"HTTP_PROXY=", "HTTPS_PROXY=", "NO_PROXY="})
	assert.True(t, found)
	assert.False(t, s.Configured())

	_, found = ProxyFromEnvironment([]string{"DOCKER_HOST=x", "PATH=C:\\x", "ALL_PROXY=http://a:1", "broken"})
	assert.False(t, found)
	_, found = ProxyFromEnvironment(nil)
	assert.False(t, found)
}

func TestWithoutAndWithProxyVariablesKeepEverythingElse(t *testing.T) {
	env := []string{"DOCKER_HOST=tcp://127.0.0.1:2376", "HTTP_PROXY=http://p:1", "https_proxy=http://p:1", "No_Proxy=localhost", "AGENT_UPDATE_ALLOW_UNSIGNED=until=2026-11-18T00:00:00Z", "PATH=C:\\a;C:\\b"}
	assert.Equal(t, []string{"DOCKER_HOST=tcp://127.0.0.1:2376", "AGENT_UPDATE_ALLOW_UNSIGNED=until=2026-11-18T00:00:00Z", "PATH=C:\\a;C:\\b"}, WithoutProxyVariables(env))
	assert.Empty(t, WithoutProxyVariables([]string{"HTTP_PROXY=x"}))

	s := ProxySettings{HTTP: "http://n:2", HTTPS: "http://n:3", NoProxy: "localhost"}
	assert.Equal(t, []string{"DOCKER_HOST=tcp://127.0.0.1:2376", "AGENT_UPDATE_ALLOW_UNSIGNED=until=2026-11-18T00:00:00Z", "PATH=C:\\a;C:\\b",
		"HTTP_PROXY=http://n:2", "HTTPS_PROXY=http://n:3", "NO_PROXY=localhost"}, WithProxyVariables(env, s))
	assert.Equal(t, WithoutProxyVariables(env), WithProxyVariables(env, ProxySettings{}))
}

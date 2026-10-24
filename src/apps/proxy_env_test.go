package apps

import (
	"runtime"
	"strings"
	"testing"

	"reagent/common"
	"reagent/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearProxyEnv gives a test a device with no proxy configured, whatever the
// machine running the suite happens to export.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy",
	} {
		t.Setenv(name, "")
	}
}

func proxyTestConfig(applianceDomain string) *config.Config {
	return &config.Config{
		ReswarmConfig:        &config.ReswarmConfig{Environment: "production", DeviceKey: 42, ApplianceDomain: applianceDomain},
		CommandLineArguments: &config.CommandLineArguments{},
	}
}

// envMap turns the emitted NAME=VALUE list into a lookup.
func envMap(envs []string) map[string]string {
	out := map[string]string{}
	for _, entry := range envs {
		name, value, found := strings.Cut(entry, "=")
		if found {
			out[name] = value
		}
	}
	return out
}

func TestResolveDeviceProxy(t *testing.T) {
	t.Run("no proxy configured", func(t *testing.T) {
		clearProxyEnv(t)
		assert.False(t, resolveDeviceProxy().enabled())
	})

	t.Run("upper case wins over lower case", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows environment variables are case-insensitive: HTTPS_PROXY and https_proxy are one variable")
		}
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://upper:3128")
		t.Setenv("https_proxy", "http://lower:3128")

		assert.Equal(t, "http://upper:3128", resolveDeviceProxy().HTTPS)
	})

	t.Run("lower case is honoured on its own", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("https_proxy", "http://lower:3128")

		proxy := resolveDeviceProxy()
		assert.True(t, proxy.enabled())
		assert.Equal(t, "http://lower:3128", proxy.HTTPS)
	})

	t.Run("an http-only proxy still counts as configured", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTP_PROXY", "http://proxy:3128")

		proxy := resolveDeviceProxy()
		assert.True(t, proxy.enabled())
		assert.Empty(t, proxy.HTTPS)
	})

	t.Run("whitespace-only is not a proxy", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "   ")

		assert.False(t, resolveDeviceProxy().enabled())
	})
}

func TestProxyEnvironmentVariables(t *testing.T) {
	t.Run("a device with no proxy injects nothing", func(t *testing.T) {
		clearProxyEnv(t)
		assert.Nil(t, proxyEnvironmentVariables(proxyTestConfig(""), "192.168.1.50"))
	})

	t.Run("every spelling the runtimes read", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTP_PROXY", "http://proxy.corp:3128")
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")

		envs := envMap(proxyEnvironmentVariables(proxyTestConfig(""), ""))

		for _, name := range []string{
			"HTTP_PROXY", "http_proxy", "IRONFLOCK_HTTP_PROXY",
			"HTTPS_PROXY", "https_proxy", "IRONFLOCK_HTTPS_PROXY",
			"NO_PROXY", "no_proxy", "IRONFLOCK_NO_PROXY",
		} {
			assert.Contains(t, envs, name, "%s is a name some runtime reads", name)
		}
		assert.Equal(t, "http://proxy.corp:3128", envs["http_proxy"])
		assert.Equal(t, envs["NO_PROXY"], envs["no_proxy"])
		assert.Equal(t, envs["NO_PROXY"], envs["IRONFLOCK_NO_PROXY"])
	})

	t.Run("an unset half is omitted, not blanked", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")

		envs := envMap(proxyEnvironmentVariables(proxyTestConfig(""), ""))

		assert.Contains(t, envs, "HTTPS_PROXY")
		assert.NotContains(t, envs, "HTTP_PROXY",
			"blanking it would override a proxy the image itself baked in")
	})

	t.Run("ALL_PROXY is never emitted", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
		t.Setenv("ALL_PROXY", "http://proxy.corp:3128")

		envs := envMap(proxyEnvironmentVariables(proxyTestConfig(""), ""))

		assert.NotContains(t, envs, "ALL_PROXY")
		assert.NotContains(t, envs, "all_proxy")
	})

	t.Run("the emitted lines survive the compose dotenv filter", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://user:pa55@proxy.corp:3128")
		t.Setenv("NO_PROXY", "internal.corp")

		envs := proxyEnvironmentVariables(proxyTestConfig(""), "192.168.1.50")
		valid, skipped := filterValidDotEnvLines(envs)

		assert.Empty(t, skipped, "a skipped key here would abort compose up for the whole app")
		assert.Len(t, valid, len(envs))
	})
}

func TestContainerNoProxy(t *testing.T) {
	entries := func(noProxy string) []string {
		return strings.Split(noProxy, ",")
	}

	t.Run("the container's own vantage point is bypassed", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128"}

		got := entries(proxy.containerNoProxy(proxyTestConfig(""), ""))

		// The device's NO_PROXY only ever says localhost, which from inside a
		// container means the container itself.
		assert.Contains(t, got, "host.docker.internal")
		assert.Contains(t, got, "172.16.0.0/12", "the docker bridge siblings sit on")
		assert.Contains(t, got, "localhost")
	})

	t.Run("the device's own exemptions come first and are kept", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128", NoProxy: "internal.corp, 10.9.9.9"}

		got := entries(proxy.containerNoProxy(proxyTestConfig(""), ""))

		require.Greater(t, len(got), 2)
		assert.Equal(t, "internal.corp", got[0])
		assert.Equal(t, "10.9.9.9", got[1], "surrounding whitespace is trimmed")
	})

	t.Run("entries are de-duplicated", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128", NoProxy: "localhost,127.0.0.1,localhost"}

		got := entries(proxy.containerNoProxy(proxyTestConfig(""), "192.168.1.50"))

		counts := map[string]int{}
		for _, entry := range got {
			counts[entry]++
			assert.NotEmpty(t, entry, "an empty entry matches nothing and confuses parsers")
		}
		assert.Equal(t, 1, counts["localhost"])
		assert.Equal(t, 1, counts["127.0.0.1"])
	})

	t.Run("the device's LAN address is bypassed", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128"}

		got := entries(proxy.containerNoProxy(proxyTestConfig(""), "192.168.1.50"))

		assert.Contains(t, got, "192.168.1.50")
	})

	t.Run("an appliance domain is bypassed, subdomains included", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128"}

		got := entries(proxy.containerNoProxy(proxyTestConfig("tls-sf015.corp.example.com"), ""))

		assert.Contains(t, got, "tls-sf015.corp.example.com")
		assert.Contains(t, got, ".tls-sf015.corp.example.com",
			"apps talk to wss://ws.<appliance domain>")
	})

	t.Run("a cloud device does not bypass its own endpoints", func(t *testing.T) {
		proxy := deviceProxy{HTTPS: "http://proxy.corp:3128"}

		got := proxy.containerNoProxy(proxyTestConfig(""), "")

		assert.NotContains(t, got, "ironflock",
			"cloud endpoints are public and DO need the proxy")
	})
}

func TestProxyEnvironmentVariablesAreOverridable(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://device-proxy.corp:3128")

	defaults := proxyEnvironmentVariables(proxyTestConfig(""), "")
	require.NotEmpty(t, defaults)

	merged := buildProdEnvironmentVariables(defaults, map[string]interface{}{
		"HTTPS_PROXY": map[string]interface{}{"value": "http://app-proxy.corp:8080"},
	})

	// Docker takes the LAST occurrence of a duplicate name.
	assert.Equal(t, "http://app-proxy.corp:8080", envMap(merged)["HTTPS_PROXY"],
		"the injected proxy is a floor, not a ceiling")
	assert.Equal(t, "http://device-proxy.corp:3128", envMap(merged)["IRONFLOCK_HTTPS_PROXY"],
		"the IRONFLOCK_ name still reports the device proxy")
}

// The failure this fixes: an app on a proxied corporate site has no outbound
// route at all, because only the agent's own service unit ever got the proxy.
func TestBuildDefaultEnvironmentVariablesCarriesTheProxy(t *testing.T) {
	app := &common.App{AppKey: 7, AppName: "myapp"}

	t.Run("a proxied device hands the proxy to its apps", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")

		envs := envMap(buildDefaultEnvironmentVariables(
			proxyTestConfig(""), common.TransitionPayload{}, common.PROD, app, ""))

		assert.Equal(t, "http://proxy.corp:3128", envs["HTTPS_PROXY"])
		assert.Equal(t, "http://proxy.corp:3128", envs["https_proxy"])
		assert.Contains(t, envs, "NO_PROXY")
	})

	t.Run("the device's LAN address reaches the bypass list", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")

		envs := envMap(buildDefaultEnvironmentVariables(
			proxyTestConfig(""), common.TransitionPayload{}, common.PROD, app, ""))

		if lanIP := envs["DEVICE_LAN_IP"]; lanIP != "" {
			assert.Contains(t, strings.Split(envs["NO_PROXY"], ","), lanIP,
				"an app reaching a port its own device publishes must not go via the proxy")
		}
	})

	t.Run("an unproxied device is untouched", func(t *testing.T) {
		clearProxyEnv(t)

		envs := envMap(buildDefaultEnvironmentVariables(
			proxyTestConfig(""), common.TransitionPayload{}, common.PROD, app, ""))

		for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
			assert.NotContains(t, envs, name, "a device with no proxy must behave exactly as before")
		}
	})
}

func TestRedactProxyURL(t *testing.T) {
	t.Run("credentials never reach the log", func(t *testing.T) {
		redacted := redactProxyURL("http://svc-account:s3cr3t@proxy.corp:3128")

		assert.NotContains(t, redacted, "s3cr3t")
		assert.Contains(t, redacted, "proxy.corp:3128", "the host is what makes the line useful")
	})

	t.Run("a credential-free proxy is left readable", func(t *testing.T) {
		assert.Equal(t, "http://proxy.corp:3128", redactProxyURL("http://proxy.corp:3128"))
	})

	t.Run("empty stays empty", func(t *testing.T) {
		assert.Empty(t, redactProxyURL(""))
	})
}

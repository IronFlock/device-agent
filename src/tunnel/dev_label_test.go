package tunnel

import (
	"strings"
	"testing"

	"reagent/common"
	"reagent/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A development run's tunnel label carries a trailing -dev, so it never
// collides with the PROD install of the same app and port. The tunnel server,
// the backend and Studio parse the same grammar; PROD labels must not change.
func TestCreateSubdomainDevLabel(t *testing.T) {
	tests := []struct {
		protocol Protocol
		stage    common.Stage
		want     string
	}{
		{HTTP, common.PROD, "12-myapp-8080"},
		{HTTPS, common.PROD, "secure-12-myapp-8080"},
		{TCP, common.PROD, "12-myapp-8080"},
		{HTTP, common.DEV, "12-myapp-8080-dev"},
		{HTTPS, common.DEV, "secure-12-myapp-8080-dev"},
		{UDP, common.DEV, "12-myapp-8080-dev"},
	}
	for _, tt := range tests {
		t.Run(string(tt.protocol)+"/"+string(tt.stage), func(t *testing.T) {
			assert.Equal(t, tt.want, CreateSubdomain(tt.protocol, 12, "MyApp", 8080, tt.stage))
		})
	}
}

func TestParseSubdomain(t *testing.T) {
	tests := []struct {
		label string
		app   string
		port  uint64
		stage common.Stage
	}{
		{"12-foo-80", "foo", 80, common.PROD},
		{"12-foo-80-dev", "foo", 80, common.DEV},
		{"secure-12-foo-80-dev", "foo", 80, common.DEV},
		{"secure-12-foo-443", "foo", 443, common.PROD},
		// The marker sits after the port: an app may well be named x-dev.
		{"12-x-dev-80", "x-dev", 80, common.PROD},
		{"12-x-dev-80-dev", "x-dev", 80, common.DEV},
		// Hyphens and digits in the app name.
		{"12-my-app-2-8080", "my-app-2", 8080, common.PROD},
		{"12-my-app-2-8080-dev", "my-app-2", 8080, common.DEV},
		{"7-80-81", "80", 81, common.PROD},
		// Proxy names (label + -protocol) parse too.
		{"12-foo-80-dev-http", "foo", 80, common.DEV},
		{"12-foo-80-tcp", "foo", 80, common.PROD},
		{"secure-12-foo-80-dev-https", "foo", 80, common.DEV},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			app, port, stage, ok := ParseSubdomain(tt.label)
			require.True(t, ok)
			assert.Equal(t, tt.app, app)
			assert.Equal(t, tt.port, port)
			assert.Equal(t, tt.stage, stage)
		})
	}

	for _, bad := range []string{"", "foo-bar", "i5-files", "12-foo", "12-foo-dev", "x12-foo-80"} {
		_, _, _, ok := ParseSubdomain(bad)
		assert.False(t, ok, bad)
	}
}

// Whatever CreateSubdomain builds, ParseSubdomain reads back unchanged, for
// both stages and every protocol, also from the proxy name.
func TestSubdomainRoundTrip(t *testing.T) {
	for _, app := range []string{"app", "x-dev", "a1-b2-3", "80", "dev"} {
		for _, protocol := range []Protocol{HTTP, HTTPS, TCP, UDP} {
			for _, stage := range []common.Stage{common.PROD, common.DEV} {
				subdomain := CreateSubdomain(protocol, 4749, app, 1883, stage)
				for _, label := range []string{subdomain, CreateTunnelID(subdomain, string(protocol))} {
					gotApp, gotPort, gotStage, ok := ParseSubdomain(label)
					require.True(t, ok, label)
					assert.Equal(t, app, gotApp, label)
					assert.Equal(t, uint64(1883), gotPort, label)
					assert.Equal(t, stage, gotStage, label)
				}
			}
		}
	}
}

// frpc.yaml does not store the stage; GetTunnelConfig reads it back from the
// proxy name, so a DEV and a PROD tunnel of the same app and port stay apart.
func TestGetTunnelConfigReadsStage(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	builder.AddTunnelConfig(TunnelConfig{Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.PROD), Protocol: HTTP, LocalPort: 41000})
	builder.AddTunnelConfig(TunnelConfig{Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.DEV), Protocol: HTTP, LocalPort: 41001})
	builder.AddTunnelConfig(TunnelConfig{Subdomain: CreateSubdomain(HTTPS, 9, "web", 8443, common.DEV), Protocol: HTTPS, LocalPort: 41002})

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 3)

	got := map[string]common.Stage{}
	for _, c := range configs {
		assert.Equal(t, "web", c.AppName)
		got[c.Name] = c.Stage
		if strings.HasSuffix(c.Name, "-dev-http") || strings.HasSuffix(c.Name, "-dev-https") {
			assert.Equal(t, common.DEV, c.Stage, c.Name)
		}
	}
	assert.Equal(t, map[string]common.Stage{
		"9-web-8080-http":             common.PROD,
		"9-web-8080-dev-http":         common.DEV,
		"secure-9-web-8443-dev-https": common.DEV,
	}, got)
}

package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"reagent/winservice"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProxyHost stands in for the store, its ACL, the service's registry
// environment and the process environment.
type fakeProxyHost struct {
	store      []byte
	storeErr   error // returned by ReadStore instead of the store
	writeErr   error
	old        []byte
	protected  bool
	protectErr error
	env        []string
	envErr     error
	setEnvErrs int // the first n SetServiceEnvironment calls fail
	setEnvRuns int
	process    map[string]string
	configured *bool
	writes     int
}

func (f *fakeProxyHost) ReadStore() ([]byte, error) {
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	if f.store == nil {
		return nil, os.ErrNotExist
	}
	return f.store, nil
}

func (f *fakeProxyHost) WriteStore(data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes++
	f.store = data
	f.protected = true
	return nil
}

func (f *fakeProxyHost) WriteOld(data []byte) error { f.old = data; return nil }

func (f *fakeProxyHost) ProtectStore() (bool, error) {
	if f.protectErr != nil {
		return false, f.protectErr
	}
	repaired := !f.protected
	f.protected = true
	return repaired, nil
}

func (f *fakeProxyHost) ServiceEnvironment() ([]string, error) { return f.env, f.envErr }

func (f *fakeProxyHost) SetServiceEnvironment(env []string) error {
	f.setEnvRuns++
	if f.setEnvRuns <= f.setEnvErrs {
		return errors.New("access denied")
	}
	f.env = env
	return nil
}

func (f *fakeProxyHost) Setenv(name, value string) error {
	if f.process == nil {
		f.process = map[string]string{}
	}
	f.process[name] = value
	return nil
}

func (f *fakeProxyHost) Unsetenv(name string) error {
	if f.process == nil {
		f.process = map[string]string{}
	}
	f.process[name] = "<unset>"
	return nil
}

func (f *fakeProxyHost) SetProxyConfigured(configured bool) error {
	f.configured = &configured
	return nil
}

// sameNoProxy leaves the stored list as the process's.
func sameNoProxy(user string) string { return user }

var (
	oldInstallEnv = []string{"HTTP_PROXY=http://user:secret@proxy:3128", "HTTPS_PROXY=http://user:secret@proxy:3128", "NO_PROXY=localhost,127.0.0.1", "DOCKER_HOST=tcp://127.0.0.1:2376"}
	oldSettings   = winservice.ProxySettings{HTTP: "http://user:secret@proxy:3128", HTTPS: "http://user:secret@proxy:3128", NoProxy: "localhost,127.0.0.1"}
)

// An install by an older agent: the proxy in the registry and in a store
// with the default ACL. The proxy stays as it was, and the ACL tightens.
func TestApplyProxyAtStartOldInstallWithStore(t *testing.T) {
	h := &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(oldSettings)), env: oldInstallEnv}
	start := applyProxyAtStart(h, sameNoProxy)

	assert.Equal(t, oldSettings, start.Settings)
	assert.Equal(t, proxySourceServiceEnv, start.Source)
	assert.True(t, start.InServiceEnv)
	assert.True(t, h.protected)
	assert.Zero(t, h.writes, "a store with the same values is not rewritten")
	assert.Equal(t, "http://user:secret@proxy:3128", h.process["HTTP_PROXY"])
	assert.Equal(t, "localhost,127.0.0.1", h.process["NO_PROXY"])
	require.NotNil(t, h.configured)
	assert.True(t, *h.configured)
}

// The store is missing (installed before proxy.env existed, or deleted): it
// is created from the registry first.
func TestApplyProxyAtStartCreatesTheStoreFromTheRegistry(t *testing.T) {
	h := &fakeProxyHost{env: oldInstallEnv}
	start := applyProxyAtStart(h, sameNoProxy)

	assert.Equal(t, oldSettings, start.Settings)
	assert.Equal(t, 1, h.writes)
	assert.Equal(t, oldSettings, winservice.ParseProxyEnv(h.store))
	assert.Nil(t, h.old)
}

// The store was edited without a reinstall: the registry's values were in
// effect and stay so; the edit is kept aside.
func TestApplyProxyAtStartRegistryWinsOverADifferentStore(t *testing.T) {
	edited := []byte("HTTP_PROXY=http://edited:1\nHTTPS_PROXY=http://edited:1\nNO_PROXY=localhost\n")
	h := &fakeProxyHost{store: edited, env: oldInstallEnv}
	start := applyProxyAtStart(h, sameNoProxy)

	assert.Equal(t, oldSettings, start.Settings)
	assert.Equal(t, edited, h.old)
	assert.Equal(t, oldSettings, winservice.ParseProxyEnv(h.store))
}

// After the move: the store alone, process environment from it.
func TestApplyProxyAtStartFromTheStore(t *testing.T) {
	s := winservice.ProxySettings{HTTP: "http://p:1", NoProxy: "localhost"}
	h := &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(s)), protected: true, env: []string{"DOCKER_HOST=tcp://127.0.0.1:2376"}}
	start := applyProxyAtStart(h, sameNoProxy)

	assert.Equal(t, s, start.Settings)
	assert.Equal(t, proxySourceStore, start.Source)
	assert.False(t, start.InServiceEnv)
	assert.Equal(t, "http://p:1", h.process["HTTP_PROXY"])
	assert.Equal(t, "<unset>", h.process["HTTPS_PROXY"])
	assert.Zero(t, h.writes)
}

// No proxy anywhere: the process environment stays as Windows gave it.
func TestApplyProxyAtStartWithoutAProxy(t *testing.T) {
	h := &fakeProxyHost{env: []string{"PATH=x"}}
	start := applyProxyAtStart(h, sameNoProxy)
	assert.Equal(t, proxySourceNone, start.Source)
	assert.Empty(t, h.process)
	require.NotNil(t, h.configured)
	assert.False(t, *h.configured)

	// An older install without a proxy wrote the three variables empty.
	h = &fakeProxyHost{env: []string{"HTTP_PROXY=", "HTTPS_PROXY=", "NO_PROXY="}}
	start = applyProxyAtStart(h, sameNoProxy)
	assert.True(t, start.InServiceEnv)
	assert.False(t, start.Settings.Configured())
	assert.Zero(t, h.writes)
	assert.Empty(t, h.process)
}

// Failures never stop the start: an unreadable store leaves the process
// environment alone, a failed write or ACL repair is logged and the values
// from the registry stay in effect.
func TestApplyProxyAtStartFailuresGoOn(t *testing.T) {
	h := &fakeProxyHost{storeErr: errors.New("access denied"), env: oldInstallEnv}
	start := applyProxyAtStart(h, sameNoProxy)
	assert.Equal(t, oldSettings, start.Settings, "the registry's values, which the process has anyway")
	assert.Zero(t, h.writes)

	h = &fakeProxyHost{storeErr: errors.New("access denied")}
	start = applyProxyAtStart(h, sameNoProxy)
	assert.Equal(t, proxySourceUnavailable, start.Source)
	assert.Empty(t, h.process)
	assert.Nil(t, h.configured, "not known: not recorded")

	h = &fakeProxyHost{env: oldInstallEnv, writeErr: errors.New("disk full"), envErr: nil}
	start = applyProxyAtStart(h, sameNoProxy)
	assert.Equal(t, oldSettings, start.Settings)
	assert.Equal(t, "http://user:secret@proxy:3128", h.process["HTTPS_PROXY"])

	h = &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(oldSettings)), protectErr: errors.New("no"), envErr: errors.New("registry")}
	start = applyProxyAtStart(h, sameNoProxy)
	assert.Equal(t, oldSettings, start.Settings)
	assert.Equal(t, proxySourceStore, start.Source)
}

func TestRemoveProxyFromServiceEnvironment(t *testing.T) {
	noSleep := func(time.Duration) {}

	h := &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(oldSettings)), env: oldInstallEnv}
	assert.True(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Equal(t, []string{"DOCKER_HOST=tcp://127.0.0.1:2376"}, h.env)

	// Nothing there: done without a write.
	h = &fakeProxyHost{env: []string{"PATH=x"}}
	assert.True(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Zero(t, h.setEnvRuns)

	// The store does not hold them (its write failed at the start): the
	// registry keeps them.
	h = &fakeProxyHost{env: oldInstallEnv}
	assert.False(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Equal(t, oldInstallEnv, h.env)
	h = &fakeProxyHost{store: []byte("HTTP_PROXY=http://other:1\n"), env: oldInstallEnv}
	assert.False(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Equal(t, oldInstallEnv, h.env)

	// Empty variables of an install without a proxy go without a store.
	h = &fakeProxyHost{env: []string{"HTTP_PROXY=", "NO_PROXY=", "PATH=x"}}
	assert.True(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Equal(t, []string{"PATH=x"}, h.env)

	// A failing write is tried three times, then given up.
	sleeps := 0
	h = &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(oldSettings)), env: oldInstallEnv, setEnvErrs: 10}
	assert.False(t, removeProxyFromServiceEnvironment(h, 3, time.Second, func(time.Duration) { sleeps++ }))
	assert.Equal(t, 3, h.setEnvRuns)
	assert.Equal(t, 2, sleeps)

	h = &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(oldSettings)), env: oldInstallEnv, setEnvErrs: 1}
	assert.True(t, removeProxyFromServiceEnvironment(h, 3, time.Second, noSleep))
	assert.Equal(t, 2, h.setEnvRuns)
}

func TestParseServiceConfigureFlags(t *testing.T) {
	o, err := parseServiceConfigureFlags([]string{"-http-proxy", "http://user@p:1", "-https-proxy", "p:2", "-proxy-password-file", "-", "-no-proxy", "a.example,b", "-no-restart"}, io.Discard)
	require.NoError(t, err)
	assert.True(t, o.Given("http-proxy"))
	assert.True(t, o.NoRestart)
	assert.Equal(t, "-", o.PasswordFile)

	o, err = parseServiceConfigureFlags([]string{"-remove-proxy"}, io.Discard)
	require.NoError(t, err)
	assert.True(t, o.Remove)
	_, err = parseServiceConfigureFlags([]string{"-no-proxy", ""}, io.Discard)
	require.NoError(t, err, "an empty list: the device's own hosts only")
	_, err = parseServiceConfigureFlags([]string{"-http-proxy", "http://u:@p:1"}, io.Discard)
	require.NoError(t, err, "the empty password takes no file")

	for name, args := range map[string][]string{
		"nothing":                     {},
		"only -no-restart":            {"-no-restart"},
		"remove with a URL":           {"-remove-proxy", "-http-proxy", "http://p:1"},
		"password in the URL":         {"-https-proxy", "http://u:secret@p:1"},
		"user without a file":         {"-http-proxy", "http://u@p:1"},
		"file for URLs without users": {"-http-proxy", "http://p:1", "-proxy-password-file", "pw"},
		"empty file name":             {"-proxy-password-file", ""},
		"bad scheme":                  {"-http-proxy", "ftp://p:1"},
		"a path":                      {"-http-proxy", "http://p:1/x"},
		"a quote":                     {"-http-proxy", "http://p:1'"},
		"empty URL":                   {"-http-proxy", ""},
		"a space in no-proxy":         {"-no-proxy", "a b"},
		"unknown flag":                {"-proxy", "http://p:1"},
		"an argument":                 {"-remove-proxy", "extra"},
	} {
		_, err := parseServiceConfigureFlags(args, io.Discard)
		require.Error(t, err, name)
		assert.Equal(t, exitUsage, exitCodeOf(err), name)
		assert.NotContains(t, err.Error(), "secret", name)
	}

	_, err = parseServiceConfigureFlags([]string{"-h"}, io.Discard)
	assert.ErrorIs(t, err, flag.ErrHelp)
	assert.Equal(t, exitOK, exitCodeOf(err))
}

func TestConfiguredProxyMergeRules(t *testing.T) {
	parse := func(args ...string) *serviceConfigureOptions {
		o, err := parseServiceConfigureFlags(args, io.Discard)
		require.NoError(t, err)
		return o
	}
	stored := winservice.ProxySettings{HTTP: "http://u:old@p:1", HTTPS: "http://q:2", NoProxy: "localhost,edited.example"}

	// A flag not given keeps its value; NO_PROXY stays.
	to, err := configuredProxy(parse("-https-proxy", "http://new:3"), stored, "")
	require.NoError(t, err)
	assert.Equal(t, winservice.ProxySettings{HTTP: "http://u:old@p:1", HTTPS: "http://new:3", NoProxy: "localhost,edited.example"}, to)

	// -no-proxy replaces the operator's own entries, as given; the agent
	// computes the rest at its start.
	to, err = configuredProxy(parse("-no-proxy", " extra.example,,10.1.0.0/16 "), stored, "")
	require.NoError(t, err)
	assert.Equal(t, "extra.example,10.1.0.0/16", to.NoProxy)
	assert.Equal(t, stored.HTTP, to.HTTP)
	to, err = configuredProxy(parse("-no-proxy", ""), stored, "")
	require.NoError(t, err)
	assert.Empty(t, to.NoProxy)

	// A first proxy has no entries of the operator's without -no-proxy.
	to, err = configuredProxy(parse("-http-proxy", "http://user@p:1", "-proxy-password-file", "pw"), winservice.ProxySettings{}, "s3cr:t")
	require.NoError(t, err)
	assert.Equal(t, winservice.ProxySettings{HTTP: "http://user:s3cr%3At@p:1"}, to)

	// The empty password stays in its form.
	to, err = configuredProxy(parse("-http-proxy", "http://user:@p:1"), stored, "")
	require.NoError(t, err)
	assert.Equal(t, "http://user:@p:1", to.HTTP)

	// A password file alone goes into the stored URLs with a user.
	to, err = configuredProxy(parse("-proxy-password-file", "pw"), stored, "new")
	require.NoError(t, err)
	assert.Equal(t, "http://u:new@p:1", to.HTTP)
	assert.Equal(t, "http://q:2", to.HTTPS)
	_, err = configuredProxy(parse("-proxy-password-file", "pw"), winservice.ProxySettings{HTTP: "http://p:1"}, "new")
	assert.Equal(t, exitPrecondition, exitCodeOf(err))

	// Nothing stored: a URL is needed.
	_, err = configuredProxy(parse("-no-proxy", "x"), winservice.ProxySettings{}, "")
	assert.Equal(t, exitPrecondition, exitCodeOf(err))

	to, err = configuredProxy(parse("-remove-proxy"), stored, "")
	require.NoError(t, err)
	assert.False(t, to.Configured())
	assert.Equal(t, winservice.ProxySettings{}, to)
}

func TestReadProxyPassword(t *testing.T) {
	pw, err := readProxyPassword(strings.NewReader("s3cret\r\nsecond line\n"), "pw")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", pw)
	pw, err = readProxyPassword(strings.NewReader("no newline"), "pw")
	require.NoError(t, err)
	assert.Equal(t, "no newline", pw)

	for name, input := range map[string]string{
		"empty":         "",
		"empty line":    "\nx",
		"control":       "a\tb\n",
		"too large":     strings.Repeat("x", maxProxyPasswordBytes+1),
		"too large nl":  strings.Repeat("x", maxProxyPasswordBytes) + "\n",
		"carriage only": "\r\n",
	} {
		_, err := readProxyPassword(strings.NewReader(input), "pw")
		assert.Error(t, err, name)
		if err != nil {
			assert.NotContains(t, err.Error(), "xxxx", name)
		}
	}

	pw, err = openProxyPassword("-", strings.NewReader("from-stdin\n"))
	require.NoError(t, err)
	assert.Equal(t, "from-stdin", pw)
	_, err = openProxyPassword("/does/not/exist", nil)
	assert.Equal(t, exitUsage, exitCodeOf(err))
}

func TestProxyReportLinesHideThePassword(t *testing.T) {
	lines := proxyReportLines(winservice.ProxySettings{HTTP: "http://user:s3cret@p:1", HTTPS: "http://u:@q:2", NoProxy: "localhost"}, `C:\ProgramData\IronFlock\DeviceAgent\proxy.env`, "localhost,127.0.0.1,::1")
	assert.Equal(t, []string{
		"proxy.configured=true",
		"proxy.http=http://user@p:1",
		"proxy.https=http://u:@q:2",
		"proxy.no_proxy=localhost",
		"proxy.no_proxy_effective=localhost,127.0.0.1,::1",
		"proxy.password=true",
		`proxy.store=C:\ProgramData\IronFlock\DeviceAgent\proxy.env`,
	}, lines)
	assert.NotContains(t, strings.Join(lines, "\n"), "s3cret")

	lines = proxyReportLines(winservice.ProxySettings{}, "x", "")
	assert.Equal(t, []string{"proxy.configured=false", "proxy.http=", "proxy.https=", "proxy.no_proxy=", "proxy.no_proxy_effective=", "proxy.password=false", "proxy.store=x"}, lines)

	assert.Equal(t, "http://user@p:1 (with a password)", redactedProxyURL("http://user:s3cret@p:1"))
	assert.Equal(t, "", redactedProxyURL(""))
}

func TestServiceCapabilitiesNameTheCommands(t *testing.T) {
	assert.Contains(t, serviceCapabilities, "service-configure")
	assert.Contains(t, serviceCapabilities, "service-proxy")
	assert.Contains(t, serviceCapabilities, "proxy-store")
	for _, c := range serviceCapabilities {
		assert.NotContains(t, c, " ")
	}
}

func TestUnderProcess(t *testing.T) {
	parent := map[uint32]uint32{100: 50, 50: 10, 10: 4, 4: 0, 7: 7}
	assert.True(t, underProcess(parent, 100, 10))
	assert.True(t, underProcess(parent, 100, 100))
	assert.False(t, underProcess(parent, 100, 7))
	assert.False(t, underProcess(parent, 7, 10), "a loop ends the walk")
	assert.False(t, underProcess(parent, 100, 0))
}

func TestExitCodeOf(t *testing.T) {
	assert.Equal(t, exitOK, exitCodeOf(nil))
	assert.Equal(t, exitFailed, exitCodeOf(errors.New("x")))
	assert.Equal(t, exitUsage, exitCodeOf(usagef("x")))
	assert.Equal(t, exitPrecondition, exitCodeOf(preconditionf("x")))
	var b bytes.Buffer
	_, _ = parseServiceConfigureFlags([]string{"-h"}, &b)
	assert.Contains(t, b.String(), "service configure")
}

// The process gets the computed list, the store keeps the operator's.
func TestApplyProxyAtStartComputesNoProxy(t *testing.T) {
	s := winservice.ProxySettings{HTTPS: "http://p:1", NoProxy: "mine.example"}
	h := &fakeProxyHost{store: []byte(winservice.RenderProxyEnv(s)), protected: true}
	var asked string
	applyProxyAtStart(h, func(user string) string { asked = user; return "localhost,127.0.0.1,::1," + user + ",10.0.0.0/24" })
	assert.Equal(t, "mine.example", asked)
	assert.Equal(t, "localhost,127.0.0.1,::1,mine.example,10.0.0.0/24", h.process["NO_PROXY"])
	assert.Equal(t, s, winservice.ParseProxyEnv(h.store), "the store is not rewritten")
}

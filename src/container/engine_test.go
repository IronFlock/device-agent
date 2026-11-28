package container

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// /proc/cpuinfo of the boards the platform meets, shortened to what counts.
const (
	// Raspberry Pi 3 or 4 with a 32-bit kernel.
	cpuinfoPi32 = "processor\t: 0\nmodel name\t: ARMv7 Processor rev 4 (v7l)\nBogoMIPS\t: 38.40\nCPU architecture: 7\nCPU variant\t: 0x0\n"
	// Raspberry Pi Zero or 1: an ARMv6 that reports 7 (the kernel quirk).
	cpuinfoPiZero = "processor\t: 0\nmodel name\t: ARMv6-compatible processor rev 7 (v6l)\nBogoMIPS\t: 697.95\nCPU architecture: 7\n"
	// Raspberry Pi 3 or 4 with a 64-bit kernel.
	cpuinfoPi64 = "processor\t: 0\nBogoMIPS\t: 108.00\nFeatures\t: fp asimd evtstrm crc32 cpuid\nCPU implementer\t: 0x41\nCPU architecture: 8\n"
)

func TestEnginePlatform(t *testing.T) {
	for name, tc := range map[string]struct {
		os, arch, machine, cpuinfo string
		want                       string
	}{
		"amd64":                               {os: "linux", arch: "amd64", machine: "x86_64", want: "linux/amd64"},
		"arm64":                               {os: "linux", arch: "arm64", machine: "aarch64", cpuinfo: cpuinfoPi64, want: "linux/arm64"},
		"32-bit engine, 32-bit kernel, v7":    {os: "linux", arch: "arm", machine: "armv7l", cpuinfo: cpuinfoPi32, want: "linux/arm/v7"},
		"32-bit engine, 32-bit kernel, v6":    {os: "linux", arch: "arm", machine: "armv6l", cpuinfo: cpuinfoPiZero, want: "linux/arm/v6"},
		"32-bit engine, 64-bit kernel":        {os: "linux", arch: "arm", machine: "aarch64", cpuinfo: cpuinfoPi64, want: "linux/arm/v8"},
		"32-bit engine, no cpuinfo, armv7l":   {os: "linux", arch: "arm", machine: "armv7l", want: "linux/arm/v7"},
		"32-bit engine, no cpuinfo, armv6l":   {os: "linux", arch: "arm", machine: "armv6l", want: "linux/arm/v6"},
		"32-bit engine, no cpuinfo, armv8l":   {os: "linux", arch: "arm", machine: "armv8l", want: "linux/arm/v8"},
		"32-bit engine, no cpuinfo, aarch64":  {os: "linux", arch: "arm", machine: "aarch64", want: "linux/arm/v8"},
		"32-bit engine, cpuinfo wins":         {os: "linux", arch: "arm", machine: "armv7l", cpuinfo: cpuinfoPiZero, want: "linux/arm/v6"},
		"32-bit engine, variant unknown":      {os: "linux", arch: "arm", want: "linux/arm"},
		"32-bit engine, cpuinfo says nothing": {os: "linux", arch: "arm", machine: "x86_64", cpuinfo: "processor\t: 0\n", want: "linux/arm"},
		"Windows containers":                  {os: "windows", arch: "amd64", want: "windows/amd64"},
		"no architecture":                     {os: "linux", want: ""},
		"no OS":                               {arch: "amd64", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, enginePlatform(tc.os, tc.arch, tc.machine, []byte(tc.cpuinfo)))
		})
	}
}

// fakeEngine is a stand-in Docker engine on this machine (a unix socket)
// that answers /_ping, /version and /info with what a real engine of the
// given kind answers.
func fakeEngine(t *testing.T, version, info string, handle func(w http.ResponseWriter, r *http.Request) bool) *Docker {
	t.Helper()
	dir, err := os.MkdirTemp("", "eng")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	engine := fakeEngineServer(version, info, handle)
	engine.Listener = listener
	engine.Start()
	t.Cleanup(engine.Close)
	return dockerAt(t, "unix://"+socket)
}

// remoteFakeEngine is fakeEngine on another machine, as DOCKER_HOST=tcp://
// names it.
func remoteFakeEngine(t *testing.T, version, info string) *Docker {
	t.Helper()
	engine := fakeEngineServer(version, info, nil)
	engine.Start()
	t.Cleanup(engine.Close)
	return dockerAt(t, "tcp://"+strings.TrimPrefix(engine.URL, "http://"))
}

func fakeEngineServer(version, info string, handle func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	return httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle != nil && handle(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.47")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(version))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(info))
		default:
			http.NotFound(w, r)
		}
	}))
}

// dockerAt is a Docker whose client negotiates its API version, as the
// agent's does, with the engine at host.
func dockerAt(t *testing.T, host string) *Docker {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return &Docker{client: cli}
}

// withCPUInfo makes the engine's CPU read cpuinfo (an error when "").
func withCPUInfo(t *testing.T, cpuinfo string) {
	t.Helper()
	restore := readCPUInfo
	readCPUInfo = func() ([]byte, error) {
		if cpuinfo == "" {
			return nil, errors.New("no /proc/cpuinfo")
		}
		return []byte(cpuinfo), nil
	}
	t.Cleanup(func() { readCPUInfo = restore })
}

func TestEngineInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		version, info, cpuinfo string
		want                   EngineInfo
	}{
		"amd64": {
			version: `{"Version":"27.0.3","Os":"linux","Arch":"amd64"}`,
			info:    `{"Architecture":"x86_64"}`,
			want:    EngineInfo{Platform: "linux/amd64", Version: "27.0.3"},
		},
		"arm64": {
			version: `{"Version":"29.1.0","Os":"linux","Arch":"arm64"}`,
			info:    `{"Architecture":"aarch64"}`,
			cpuinfo: cpuinfoPi64,
			want:    EngineInfo{Platform: "linux/arm64", Version: "29.1.0"},
		},
		"32-bit engine on a 32-bit kernel, v7": {
			version: `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`,
			info:    `{"Architecture":"armv7l"}`,
			cpuinfo: cpuinfoPi32,
			want:    EngineInfo{Platform: "linux/arm/v7", Version: "28.5.2"},
		},
		"32-bit engine on a 32-bit kernel, v6": {
			version: `{"Version":"20.10.24","Os":"linux","Arch":"arm"}`,
			info:    `{"Architecture":"armv6l"}`,
			cpuinfo: cpuinfoPiZero,
			want:    EngineInfo{Platform: "linux/arm/v6", Version: "20.10.24"},
		},
		"32-bit engine on a 64-bit kernel": {
			version: `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`,
			info:    `{"Architecture":"aarch64"}`,
			cpuinfo: cpuinfoPi64,
			want:    EngineInfo{Platform: "linux/arm/v8", Version: "28.5.2"},
		},
		"32-bit engine, no cpuinfo, /info tells": {
			version: `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`,
			info:    `{"Architecture":"armv7l"}`,
			want:    EngineInfo{Platform: "linux/arm/v7", Version: "28.5.2"},
		},
		"32-bit engine, nothing tells the variant": {
			version: `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`,
			info:    `{}`,
			want:    EngineInfo{Platform: "linux/arm", Version: "28.5.2"},
		},
		// Docker Desktop on an Intel Mac or Windows PC: the engine's VM counts,
		// whatever the agent was built for.
		"Docker Desktop": {
			version: `{"Version":"28.5.1","Os":"linux","Arch":"amd64"}`,
			info:    `{"Architecture":"x86_64","OperatingSystem":"Docker Desktop"}`,
			want:    EngineInfo{Platform: "linux/amd64", Version: "28.5.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			withCPUInfo(t, tc.cpuinfo)
			docker := fakeEngine(t, tc.version, tc.info, nil)

			got, err := docker.EngineInfo(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A failing /info leaves only the variant out.
func TestEngineInfoWithoutInfo(t *testing.T) {
	withCPUInfo(t, "")
	docker := fakeEngine(t, `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`, "", func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/info") {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return true
		}
		return false
	})

	got, err := docker.EngineInfo(context.Background())

	require.NoError(t, err)
	assert.Equal(t, EngineInfo{Platform: "linux/arm", Version: "28.5.2"}, got)
}

func TestEngineInfoDockerAbsent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host := "tcp://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	_, err = dockerAt(t, host).EngineInfo(context.Background())

	assert.Error(t, err)
}

// A hung daemon costs the caller no more than its timeout.
func TestEngineInfoDockerHangs(t *testing.T) {
	release := make(chan struct{})
	docker := fakeEngine(t, "", "", func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := docker.EngineInfo(ctx)

	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// An engine on another machine: this machine's CPU says nothing about it,
// only the engine's own /info does.
func TestEngineInfoRemoteEngine(t *testing.T) {
	withCPUInfo(t, cpuinfoPiZero)
	docker := remoteFakeEngine(t, `{"Version":"28.5.2","Os":"linux","Arch":"arm"}`, `{"Architecture":"armv7l"}`)

	got, err := docker.EngineInfo(context.Background())

	require.NoError(t, err)
	assert.Equal(t, EngineInfo{Platform: "linux/arm/v7", Version: "28.5.2"}, got)
}

func TestEngineIsLocal(t *testing.T) {
	for host, want := range map[string]bool{
		"unix:///var/run/docker.sock":    true,
		"npipe:////./pipe/docker_engine": true,
		"tcp://192.168.1.20:2375":        false,
		"ssh://pi@192.168.1.20":          false,
		"http://localhost:2375":          false,
	} {
		assert.Equal(t, want, engineIsLocal(host), host)
	}
}

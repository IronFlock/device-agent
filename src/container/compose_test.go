package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"reagent/common"
	"reagent/config"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCompose builds a Compose without going through NewCompose, which
// shells out to `docker compose` via IsComposeSupported(). Tests that exercise
// pure parse/format helpers set Supported explicitly and never touch a daemon.
func newTestCompose(cfg *config.Config, supported bool) *Compose {
	c := &Compose{config: cfg}
	c.SetSupported(supported)
	return c
}

func TestComposeListImages(t *testing.T) {
	c := newTestCompose(builders.DefaultTestConfig(), false)

	t.Run("extracts image names from services", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": map[string]interface{}{
				"web": map[string]interface{}{
					"image": "nginx:1.25",
				},
				"db": map[string]interface{}{
					"image": "postgres:16",
				},
			},
		}

		images, err := c.ListImages(dc)
		require.NoError(t, err)

		sort.Strings(images)
		assert.Equal(t, []string{"nginx:1.25", "postgres:16"}, images)
	})

	t.Run("skips services without an image (build-only)", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": map[string]interface{}{
				"builder": map[string]interface{}{
					"build": "./Dockerfile",
				},
				"web": map[string]interface{}{
					"image": "nginx:latest",
				},
			},
		}

		images, err := c.ListImages(dc)
		require.NoError(t, err)
		assert.Equal(t, []string{"nginx:latest"}, images)
	})

	t.Run("non-string image is stringified via fmt.Sprint", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": map[string]interface{}{
				"weird": map[string]interface{}{
					"image": 1234,
				},
			},
		}

		images, err := c.ListImages(dc)
		require.NoError(t, err)
		assert.Equal(t, []string{"1234"}, images)
	})

	t.Run("empty services yields empty, non-nil slice", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": map[string]interface{}{},
		}

		images, err := c.ListImages(dc)
		require.NoError(t, err)
		require.NotNil(t, images)
		assert.Empty(t, images)
	})

	t.Run("missing services key errors", func(t *testing.T) {
		images, err := c.ListImages(map[string]interface{}{})
		require.Error(t, err)
		assert.Nil(t, images)
		assert.Contains(t, err.Error(), "failed to infer services")
	})

	t.Run("services of wrong type errors", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": "not-a-map",
		}
		images, err := c.ListImages(dc)
		require.Error(t, err)
		assert.Nil(t, images)
		assert.Contains(t, err.Error(), "failed to infer services")
	})

	t.Run("service of wrong type errors", func(t *testing.T) {
		dc := map[string]interface{}{
			"services": map[string]interface{}{
				"bad": "not-a-map",
			},
		}
		images, err := c.ListImages(dc)
		require.Error(t, err)
		assert.Nil(t, images)
		assert.Contains(t, err.Error(), "failed to infer service")
	})
}

func TestComposeHasComposeDir(t *testing.T) {
	base := t.TempDir()
	composeDir := filepath.Join(base, "compose")
	buildDir := filepath.Join(base, "build")
	require.NoError(t, os.MkdirAll(composeDir, 0o755))
	require.NoError(t, os.MkdirAll(buildDir, 0o755))

	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.AppsComposeDir = composeDir
	cfg.CommandLineArguments.AppsBuildDir = buildDir

	c := newTestCompose(cfg, false)

	// PROD stage resolves against AppsComposeDir.
	require.NoError(t, os.MkdirAll(filepath.Join(composeDir, "myapp"), 0o755))
	// DEV stage resolves against AppsBuildDir.
	require.NoError(t, os.MkdirAll(filepath.Join(buildDir, "devapp"), 0o755))

	tests := []struct {
		name    string
		appName string
		stage   common.Stage
		want    bool
	}{
		{"prod app present in compose dir", "myapp", common.PROD, true},
		{"prod app absent", "ghost", common.PROD, false},
		{"dev app present in build dir", "devapp", common.DEV, true},
		{"dev app absent", "devghost", common.DEV, false},
		{"prod stage does not look in build dir", "devapp", common.PROD, false},
		{"dev stage does not look in compose dir", "myapp", common.DEV, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.HasComposeDir(tt.appName, tt.stage))
		})
	}
}

// When compose is unsupported the daemon-touching calls must short-circuit to
// empty results without error (and without shelling out to docker).
func TestComposeUnsupportedShortCircuits(t *testing.T) {
	c := newTestCompose(builders.DefaultTestConfig(), false)

	t.Run("Status returns empty slice, no error", func(t *testing.T) {
		statuses, err := c.Status("/tmp/does-not-matter.yml")
		require.NoError(t, err)
		assert.Empty(t, statuses)
	})

	t.Run("List returns empty slice, no error", func(t *testing.T) {
		entries, err := c.List()
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

// composeStarted decides when WaitForRunning ends a start: a clean exit is a
// one-shot service that finished, not a failure.
func TestComposeStarted(t *testing.T) {
	st := func(service, state string, exitCode int) ComposeStatus {
		return ComposeStatus{Service: service, State: state, ExitCode: exitCode}
	}

	tests := []struct {
		name     string
		statuses []ComposeStatus
		started  bool
		errMsg   string
	}{
		{"no containers yet keeps waiting", nil, false, ""},
		{"all running", []ComposeStatus{st("web", "running", 0), st("db", "running", 0)}, true, ""},
		{"one-shot exited 0 next to a running service", []ComposeStatus{st("migrate", "exited", 0), st("web", "running", 0)}, true, ""},
		{"exited non-zero fails", []ComposeStatus{st("migrate", "exited", 3), st("web", "running", 0)}, false, `service "migrate" exited with code 3`},
		{"dead fails", []ComposeStatus{st("web", "dead", 0), st("db", "running", 0)}, false, `service "web" is dead`},
		{"failure wins over a pending container", []ComposeStatus{st("web", "created", 0), st("db", "exited", 1)}, false, `service "db" exited with code 1`},
		{"created keeps waiting", []ComposeStatus{st("web", "created", 0), st("db", "running", 0)}, false, ""},
		{"restarting keeps waiting", []ComposeStatus{st("web", "restarting", 0), st("migrate", "exited", 0)}, false, ""},
		{"everything exited 0 fails", []ComposeStatus{st("migrate", "exited", 0), st("seed", "exited", 0)}, false, "all services have exited, at least one has to keep running"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started, err := composeStarted(tt.statuses)
			if tt.errMsg != "" {
				require.EqualError(t, err, tt.errMsg)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.started, started)
		})
	}
}

// parseComposePSOutput must accept every output shape `docker compose ps -a
// --format json` has produced across compose versions: a single JSON array
// (<= 2.20), NDJSON objects (>= 2.21), and blank output for absent projects.
// These fixtures characterize the jq pipeline the previous implementation
// shelled out to.
func TestParseComposePSOutput(t *testing.T) {
	obj := func(name, state string) string {
		return `{"ID":"id-` + name + `","Name":"` + name + `","Service":"` + name + `","State":"` + state + `"}`
	}

	t.Run("NDJSON objects (compose >= 2.21)", func(t *testing.T) {
		statuses, err := parseComposePSOutput([]byte(obj("web", "running") + "\n" + obj("db", "exited") + "\n"))
		require.NoError(t, err)
		require.Len(t, statuses, 2)
		assert.Equal(t, "web", statuses[0].Name)
		assert.Equal(t, "running", statuses[0].State)
		assert.Equal(t, "db", statuses[1].Name)
		assert.Equal(t, "exited", statuses[1].State)
	})

	t.Run("single JSON array (compose <= 2.20)", func(t *testing.T) {
		statuses, err := parseComposePSOutput([]byte("[" + obj("web", "running") + "," + obj("db", "running") + "]\n"))
		require.NoError(t, err)
		require.Len(t, statuses, 2)
		assert.Equal(t, "web", statuses[0].Name)
		assert.Equal(t, "db", statuses[1].Name)
	})

	t.Run("mixed stream of arrays and objects", func(t *testing.T) {
		statuses, err := parseComposePSOutput([]byte("[" + obj("a", "running") + "]\n" + obj("b", "dead") + "\n"))
		require.NoError(t, err)
		require.Len(t, statuses, 2)
		assert.Equal(t, "a", statuses[0].Name)
		assert.Equal(t, "b", statuses[1].Name)
	})

	t.Run("blank output yields empty slice", func(t *testing.T) {
		for _, input := range []string{"", "\n", "  \n\n"} {
			statuses, err := parseComposePSOutput([]byte(input))
			require.NoError(t, err)
			assert.Empty(t, statuses)
			assert.NotNil(t, statuses)
		}
	})

	t.Run("invalid JSON errors", func(t *testing.T) {
		_, err := parseComposePSOutput([]byte("not-json"))
		require.Error(t, err)
	})
}

// ComposeStatus is the JSON contract for `docker compose ps`. Verify the struct
// tags by round-tripping a representative payload through json.Unmarshal — this
// is exactly what Status() does with the daemon output.
func TestComposeStatusUnmarshal(t *testing.T) {
	raw := `{
		"Command": "/docker-entrypoint.sh",
		"CreatedAt": "2024-01-01 00:00:00 +0000 UTC",
		"ExitCode": 0,
		"Health": "healthy",
		"ID": "abc123",
		"Image": "nginx:1.25",
		"Name": "prod_1_web",
		"Project": "prod_1_web",
		"Service": "web",
		"State": "running",
		"Status": "Up 5 minutes",
		"Publishers": [
			{"URL": "0.0.0.0", "TargetPort": 80, "PublishedPort": 8080, "Protocol": "tcp"}
		]
	}`

	var status ComposeStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &status))

	assert.Equal(t, "abc123", status.ID)
	assert.Equal(t, "nginx:1.25", status.Image)
	assert.Equal(t, "web", status.Service)
	assert.Equal(t, "running", status.State)
	assert.Equal(t, "healthy", status.Health)
	assert.Equal(t, 0, status.ExitCode)
	assert.Equal(t, "prod_1_web", status.Name)

	require.Len(t, status.Publishers, 1)
	assert.Equal(t, "0.0.0.0", status.Publishers[0].URL)
	assert.Equal(t, 80, status.Publishers[0].TargetPort)
	assert.Equal(t, 8080, status.Publishers[0].PublishedPort)
	assert.Equal(t, "tcp", status.Publishers[0].Protocol)
}

// ComposeListEntry is the JSON contract for `docker compose ls`.
func TestComposeListEntryUnmarshal(t *testing.T) {
	raw := `[
		{"Name": "prod_1_web", "Status": "running(1)", "ConfigFiles": "/apps/compose/web/docker-compose.yml"},
		{"Name": "dev_2_db", "Status": "exited(1)", "ConfigFiles": "/apps/build/db/docker-compose.yml"}
	]`

	var entries []ComposeListEntry
	require.NoError(t, json.Unmarshal([]byte(raw), &entries))

	require.Len(t, entries, 2)
	assert.Equal(t, "prod_1_web", entries[0].Name)
	assert.Equal(t, "running(1)", entries[0].Status)
	assert.Equal(t, "/apps/compose/web/docker-compose.yml", entries[0].ConfigFiles)
	assert.Equal(t, "dev_2_db", entries[1].Name)
	assert.Equal(t, "/apps/build/db/docker-compose.yml", entries[1].ConfigFiles)
}

// DockerCompose / Service model a parsed compose file. Verify the json tags
// map the expected keys so callers that unmarshal a compose file get the right
// fields.
func TestDockerComposeUnmarshal(t *testing.T) {
	raw := `{
		"version": "3.8",
		"services": {
			"web": {
				"image": "nginx:1.25",
				"ports": ["8080:80"],
				"environment": ["FOO=bar"]
			},
			"builder": {
				"build": "./svc"
			}
		}
	}`

	var dc DockerCompose
	require.NoError(t, json.Unmarshal([]byte(raw), &dc))

	assert.Equal(t, "3.8", dc.Version)
	require.Contains(t, dc.Services, "web")
	assert.Equal(t, "nginx:1.25", dc.Services["web"].Image)
	assert.Equal(t, []string{"8080:80"}, dc.Services["web"].Ports)
	assert.Equal(t, []string{"FOO=bar"}, dc.Services["web"].Environment)

	require.Contains(t, dc.Services, "builder")
	assert.Equal(t, "./svc", dc.Services["builder"].Build)
	assert.Empty(t, dc.Services["builder"].Image)
}

// =============================================================================
// Command plumbing: a non-zero exit must carry the reason the CLI printed, and
// an unconsumed output stream must never stall the command.
// =============================================================================

// newFakeComposeCompose returns a Compose whose "docker" is the given shell
// script, so the exit-code/output plumbing is exercised without a daemon.
func newFakeComposeCompose(t *testing.T, script string) *Compose {
	t.Helper()

	path := filepath.Join(t.TempDir(), "fake-docker")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))

	c := newTestCompose(builders.DefaultTestConfig(), true)
	c.binary = path
	return c
}

func TestComposeErrorNamesSubcommandAndQuotesOutput(t *testing.T) {
	cause := errors.New("exit status 1")
	err := &ComposeError{Subcommand: "up", Output: "Bind for 0.0.0.0:40001 failed: port is already allocated", Err: cause}

	assert.Equal(t,
		"docker compose up failed: exit status 1: Bind for 0.0.0.0:40001 failed: port is already allocated",
		err.Error(),
	)
	// Callers classify by string match on the wrapped chain (isPortAllocationError).
	assert.ErrorIs(t, err, cause)

	bare := &ComposeError{Subcommand: "stop", Err: cause}
	assert.Equal(t, "docker compose stop failed: exit status 1", bare.Error())
}

func TestComposeOutputTailKeepsTheHeadAndTheLastLines(t *testing.T) {
	t.Run("short output is rendered once, in order", func(t *testing.T) {
		tail := &composeOutputTail{}
		for i := 0; i < composeTailLines; i++ {
			tail.add(fmt.Sprintf("line-%d", i))
		}

		joined := tail.String()
		assert.Len(t, strings.Split(joined, "; "), composeTailLines)
		assert.True(t, strings.HasPrefix(joined, "line-0; line-1;"), "no line is repeated from the head: %s", joined)
		assert.NotContains(t, joined, "omitted")
	})

	t.Run("long output keeps the head, marks the gap and keeps the tail", func(t *testing.T) {
		tail := &composeOutputTail{}
		total := composeTailLines + composeHeadLines + 10
		for i := 0; i < total; i++ {
			tail.add(fmt.Sprintf("line-%d", i))
		}

		joined := tail.String()
		parts := strings.Split(joined, "; ")
		require.Len(t, parts, composeHeadLines+1+composeTailLines)

		for i := 0; i < composeHeadLines; i++ {
			assert.Equal(t, fmt.Sprintf("line-%d", i), parts[i], "the first lines are kept")
		}
		assert.Equal(t, "(10 line(s) omitted)", parts[composeHeadLines])
		assert.Equal(t, fmt.Sprintf("line-%d", total-composeTailLines), parts[composeHeadLines+1], "the tail starts right after the gap")
		assert.Equal(t, fmt.Sprintf("line-%d", total-1), parts[len(parts)-1], "the newest line is kept")
	})

	t.Run("a head line still inside the tail is not repeated", func(t *testing.T) {
		tail := &composeOutputTail{}
		for i := 0; i < composeTailLines+2; i++ {
			tail.add(fmt.Sprintf("line-%d", i))
		}

		parts := strings.Split(tail.String(), "; ")
		require.Len(t, parts, composeTailLines+2, "two evicted lines come from the head, nothing is omitted")
		assert.Equal(t, "line-0", parts[0])
		assert.Equal(t, "line-1", parts[1])
		assert.Equal(t, "line-2", parts[2])
		assert.NotContains(t, tail.String(), "omitted")
	})

	t.Run("a kept line is capped", func(t *testing.T) {
		tail := &composeOutputTail{}
		tail.add(strings.Repeat("x", composeLineMaxBytes*4))

		assert.Less(t, len(tail.String()), composeLineMaxBytes+8)
		assert.True(t, strings.HasSuffix(tail.String(), "…"))
	})
}

// A crash of the CLI binary itself (Go runtime abort) prints its reason FIRST,
// then hundreds of stack and register lines. The error must carry that first
// line, not just the registers at the end.
func TestComposeErrorCarriesTheReasonOfACrashedCLI(t *testing.T) {
	c := newFakeComposeCompose(t, `echo "panic: runtime error: invalid memory address" >&2
echo "goroutine 1 [running]:" >&2
i=0; while [ $i -lt 200 ]; do echo "  /usr/local/go/src/runtime/proc.go:$i +0x67 fp=0xc0000687e0" >&2; i=$((i+1)); done
echo "rax 0x0; rip 0x7f32f088195c; rflags 0x246; cs 0x33" >&2
exit 2`)

	err := c.Stop("/tmp/does-not-matter.yml")

	var composeErr *ComposeError
	require.ErrorAs(t, err, &composeErr)
	assert.Contains(t, err.Error(), "exit status 2")
	assert.Contains(t, err.Error(), "panic: runtime error: invalid memory address", "the crash reason from the head survives")
	assert.Contains(t, err.Error(), "line(s) omitted")
	assert.Contains(t, err.Error(), "rflags 0x246", "the tail is still there")
}

// A failing stop/rm/down is run for effect and nothing consumes its stream;
// the reason must still reach the caller instead of a bare "exit status 1".
func TestComposeRunSurfacesTheCLIReason(t *testing.T) {
	c := newFakeComposeCompose(t, `echo "no configuration file provided: not found" >&2; exit 1`)

	err := c.Stop("/tmp/does-not-matter.yml")

	var composeErr *ComposeError
	require.ErrorAs(t, err, &composeErr)
	assert.Equal(t, "stop", composeErr.Subcommand)
	assert.Contains(t, err.Error(), "no configuration file provided: not found")
}

func TestComposeRunSucceedsSilently(t *testing.T) {
	c := newFakeComposeCompose(t, `echo "Container app-web  Stopped"; exit 0`)

	assert.NoError(t, c.Stop("/tmp/does-not-matter.yml"))
	assert.NoError(t, c.Remove("/tmp/does-not-matter.yml"))
	assert.NoError(t, c.Down("/tmp/does-not-matter.yml"))
}

// An unconsumed stream must not block its reader: a blocked reader stops the
// pipe from being drained, which deadlocks the CLI once its output outgrows
// the pipe buffer and hangs Wait forever.
func TestComposeUnconsumedStreamDoesNotStall(t *testing.T) {
	c := newFakeComposeCompose(t, `i=0; while [ $i -lt 20000 ]; do echo "chatter line $i"; i=$((i+1)); done; echo "boom" >&2; exit 1`)

	// Deliberately drop the channel, exactly as an unread stream would.
	_, cmd, err := c.composeCommandContext(context.Background(), "/tmp/does-not-matter.yml", "up")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		require.Error(t, waitErr)
		assert.Contains(t, waitErr.Error(), "boom", "the tail survives the dropped stream")
	case <-time.After(30 * time.Second):
		t.Fatal("Wait blocked on an unconsumed output stream")
	}
}

// The app's .env-compose must be passed as --env-file whenever it exists next
// to the compose file: it is what makes ${VAR} interpolation in the compose
// file (volume driver_opts, ports) resolve to device-level app parameters.
// Without the file the flag must be dropped entirely — compose rejects an
// --env-file that does not exist.
func TestComposeFileArgsPassesEnvFileWhenPresent(t *testing.T) {
	appDir := t.TempDir()
	composePath := filepath.Join(appDir, "docker-compose.json")

	t.Run("no .env-compose: only -f", func(t *testing.T) {
		assert.Equal(t, []string{"-f", composePath}, composeFileArgs(composePath))
	})

	t.Run(".env-compose next to the compose file: --env-file follows -f", func(t *testing.T) {
		envPath := filepath.Join(appDir, DotEnvFileName)
		require.NoError(t, os.WriteFile(envPath, []byte("SHARE_HOST=nas.local\n"), 0o644))

		assert.Equal(t, []string{"-f", composePath, "--env-file", envPath}, composeFileArgs(composePath))
	})
}

// End-to-end over the argv actually handed to the CLI: the env file rides
// along on every subcommand (interpolation happens on file load for all of
// them), placed before the subcommand where compose expects root flags.
func TestComposeCommandArgvIncludesEnvFile(t *testing.T) {
	c := newFakeComposeCompose(t, `printf '%s\n' "$*"`)

	appDir := t.TempDir()
	composePath := filepath.Join(appDir, "docker-compose.json")
	envPath := filepath.Join(appDir, DotEnvFileName)
	require.NoError(t, os.WriteFile(envPath, []byte("SHARE_HOST=nas.local\n"), 0o644))

	outputChan, cmd, err := c.composeCommandContext(context.Background(), composePath, "up", "-d")
	require.NoError(t, err)

	var argv []string
	for line := range outputChan {
		argv = append(argv, line)
	}
	require.NoError(t, cmd.Wait())

	require.Len(t, argv, 1)
	assert.Equal(t,
		strings.Join([]string{"compose", "-f", composePath, "--env-file", envPath, "up", "-d"}, " "),
		argv[0],
	)
}

// A single line longer than bufio.Scanner's 64 KB default must not end the
// scan: that would stop the pipe being drained and hang the command.
func TestComposeLongOutputLineDoesNotStall(t *testing.T) {
	// head | tr writes the line in linear time; an awk concatenation loop is
	// quadratic and on a loaded CI runner alone outlasted the 30s below.
	c := newFakeComposeCompose(t, `head -c 200000 /dev/zero | tr '\0' x; echo; echo "tail marker" >&2; exit 1`)

	outputChan, cmd, err := c.composeCommandContext(context.Background(), "/tmp/does-not-matter.yml", "up")
	require.NoError(t, err)

	lines := make([]string, 0, 2)
	go func() {
		for line := range outputChan {
			lines = append(lines, line)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		require.Error(t, waitErr)
		assert.Contains(t, waitErr.Error(), "tail marker")
	case <-time.After(30 * time.Second):
		t.Fatal("Wait blocked on an over-long output line")
	}
}

# Testing guide (reagent)

How tests are structured in this module and how to add to them. Read this before
writing a new test.

## Running tests

All recipes live in the repo-root `Justfile` and run from `src/`:

| Command | What it does |
|---|---|
| `just test` | Unit tests as CI runs them (`-race -shuffle=on -count=1`). Integration-tagged tests are excluded. |
| `just test-short` | Unit tests, fast (`go test -short ./...`), for quick iteration. |
| `just test-integration` | Integration tests (`-tags integration`). Need external resources — see below. |
| `just test-coverage` | Unit tests + HTML coverage report at `src/coverage.html`. |
| `just test-generate-mocks` | Regenerate the mockery mocks (see below). |

`build-frpc` is a prerequisite of the test recipes: `./...` compiles the
`api` and `system` packages, which `//go:embed` the frpc binary, so the build
fails without `src/embedded/frpc_binary`. The recipe builds frpc for this host
from the `frp/` module (Go's build cache keeps repeated runs quick).

## Test doubles — where they live

There is one home for shared test doubles: `reagent/testutil`, split by kind.

| Package | Kind | Use when |
|---|---|---|
| `testutil/builders` | Pure-data builders (config, app, payload) | You need a valid fixture. `builders.DefaultTestConfig()` is the single source of truth for test config; package-local `testConfig()` helpers should delegate to it. |
| `testutil/fakes` | Hand-written, stateful in-memory doubles | Behavior matters more than call expectations — e.g. `fakes.Messenger` records calls, serves configured per-topic responses, and can `SimulateDisconnect()`. |
| `testutil/mocks` | mockery-generated (testify/mock) | You want to assert "method X was called with Y" or inject errors per call. `mocks.Container`, `mocks.Database`, `mocks.Messenger`, `mocks.Network`, `mocks.TunnelManager`. |

Doubles that wrap **unexported** internals of the package under test stay local
as `*_test.go` in that package (e.g. the WAMP `MockNexusClient` reconnect
simulator in `messenger/mock_nexus_client_test.go`). Never put mock code in a
non-`_test.go` file — it would ship in the production binary.

### Mocks vs. concrete structs

mockery can only generate mocks from **interfaces**. These are mockable:
`container.Container`, `messenger.Messenger`, `persistence.Database`,
`network.Network`, `tunnel.TunnelManager`.

These are **concrete structs** and are NOT mockable — test them with
real-but-isolated dependencies instead:

- `store.AppStore` → construct it with a real in-memory `persistence` DB and a
  `fakes.Messenger`.
- `persistence` `AppStateDatabase` → use an in-memory / `t.TempDir()` sqlite DB
  (the driver is pure-Go `modernc.org/sqlite`, so no external service).
- `apps.{StateMachine,StateObserver,AppManager}` → construct concretely with a
  `mocks.Container`, in-memory store, and `fakes.Messenger`.

If `api`/`apps` ever need to inject a *fake* store, introduce a narrow interface
in `store` at that point — do not add a mock for the concrete struct.

### Regenerating mocks

Mocks are generated from `src/.mockery.yaml` (mockery v3, pinned in the
`Justfile`). After changing a mocked interface, run `just test-generate-mocks`
and commit the regenerated files under `src/testutil/mocks/`. Generation is
deterministic — re-running with no interface change produces no diff.

## Conventions

- **Assertions:** use `github.com/stretchr/testify` everywhere. `require` for
  preconditions and anything that should stop the test on failure; `assert` for
  soft checks where the test can continue. testify's argument order is
  `(expected, actual)`.
- **Table-driven tests + subtests:** prefer a `[]struct{...}` table iterated with
  `t.Run(tt.name, ...)`. See `apps/state_machine_test.go` and
  `api/request_app_state_test.go`.
- **Filesystem/IO:** use `t.TempDir()`; never write to a fixed path.
- **Naming:** `*_test.go`, co-located with the code under test (same package for
  white-box tests, `package foo_test` for black-box tests that import
  `testutil`).

## Build tags — unit vs. integration

Tests that need a live external resource (frps server, Docker daemon, NetworkManager
D-Bus) must be guarded with a build tag so they never run under `just test`:

```go
//go:build integration

package tunnel
```

`just test` (no tag) does not compile them; `just test-integration` does. The
tunnel test (`tunnel/tunnel_test.go`) is the reference example.

## Adding tests for a new package

1. Add `foo/foo_test.go` (`package foo` or `package foo_test`).
2. Provide dependencies via `testutil`: builders for fixtures, `mocks.*` for
   interface call-assertions, `fakes.*`/in-memory sqlite for stateful deps.
3. Run `just test`. No package list to update — `./...` discovers it
   automatically.

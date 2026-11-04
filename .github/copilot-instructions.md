# DeviceManagementAgent - AI Coding Instructions

## Overview
Go-based agent running on IoT devices. Manages containers, handles WAMP communication, provides tunneling via embedded frpc.

## The lifeline rule: the agent must always start and connect

The agent's connection to IronFlock (ironflock.com, or the IronFlock appliance
the device belongs to) is the only lifeline to a device in the field. Every
fix reaches the device through it. An agent that does not start, or starts
and never connects, can be fixed only by a person at the device, and field
devices are unattended.

**Every code change gets a lifeline review.** Before you hand over a change
(a diff, a pull request, a release), review it for one question: can it stop
the agent from starting, from connecting, or from reconnecting after the
connection broke? Ask it for every target in `targets` and every install kind
(the Linux layouts `ironflock`, `legacy`, `legacy-image`, and the Windows
service). Report the result with the change: which parts of the path below
the change touches, why it is safe, and how you tested it. If it touches none,
say so. If you find a risk, fix it or stop and ask. Never hand over a change
that can cut the lifeline without saying so.

The path, in start order:

1. **Start:** `main.go` (`main`, `runAgent`, the single-instance lock,
   `applyResourceProtection` in `resources_*.go`), the Windows service
   (`service_windows.go`, `winservice`), `filesystem.InitDirectories`,
   `config.LoadReswarmConfig` (the `.flock` file), `layout`.
2. **Before the connection:** `NewAgent` in `agent.go` up to
   `messenger.NewWampSession`: database (`persistence`), Docker (`container`),
   tunnel manager, NetworkManager, disk guard. An error or a `log.Fatal` here
   stops the agent before it connects.
3. **Connection:** `messenger` (dial, backoff, heartbeat, reconnect,
   `OnConnect`), `trust` (TLS roots), the endpoint and credentials in the
   `.flock` file.
4. **Recovery:** the update check at start (`checkForUpdateAtStart`,
   `system`), `selfupdate` (probation, rollback), `updatesig`, `lifecycle`,
   the layout migration (`migrate`, `migratestate`), and the service unit and
   `repair.sh` that `service install` writes (`linuxservice`, `servicecmd_*`).

A change fails the review when it:

- stops the start or the connection on an error the agent can live without.
  A feature that fails logs the error, and the agent goes on (example: the
  Authenticode self-check runs in the background and never stops the agent).
- adds work before the connection that can block without a timeout (Docker,
  network, file locks, a full disk).
- starts a goroutine without `safe.Go`, so that a panic in it kills the agent.
- makes a retry give up, or a backoff grow without limit. Connection retries
  never stop.
- updates, migrates or restarts the agent while it runs without a user asking
  for it, or makes a change that cannot be undone while the agent is not
  connected (see "Updates never cost a device its connection to IronFlock" in
  README.md).
- changes the `.flock` format, the database schema, the folder layout or the
  service unit so that the previous version cannot start on what the new one
  leaves behind. A failed update rolls back to the previous version, which
  must still start and connect.
- needs more memory, CPU or disk at start, so that a small device
  (linux/arm/6) cannot run it.
- breaks the build of a target in `targets`.

How to test:

- `just lint` (builds and lints for Linux and Windows) and `just test`.
- Run the changed agent (`just run`, `just run_mac`), see `Connected!` in the
  log, break the connection (network or router), and see it reconnect.
- When the change touches the start, the update, the rollback or the
  migration: device-init's end-to-end tests on fresh cloud VMs (`just e2e` in
  device-init; `TestBrokenUpdates` uses the `release.TestFault` builds
  `crash` and `offline`).

## Prerequisites
```bash
# Download frpc binary (required before running)
make download_frpc
```

## Commands
```bash
make run          # Run locally (Linux)
make run_mac      # Run on macOS
make test         # Unit tests (packages without frpc dependency)
make test-all     # All tests (requires frpc binary)
make test-coverage    # Coverage report
make build-all-docker # Build for all target platforms
```

## Project Structure
```
src/
├── main.go           # Entry point
├── agent.go          # Core agent logic
├── api/              # WAMP RPC handlers
├── apps/             # Container management (Docker)
├── messenger/        # WAMP client abstraction
├── tunnel/           # frpc integration
├── config/           # Configuration parsing (.flock files)
├── networkmanager/   # Linux NetworkManager API
├── persistence/      # Local database (SQLite)
└── embedded/         # Embedded frpc binary
```

## Configuration
Agent requires a `.flock` file with device credentials:
```bash
./reagent -config path/to/config.flock -prettyLogging
```

Development uses `/opt/reagent` as agent directory with `test-config.flock`.

## Build Targets
Defined in `targets` file:
```
linux/amd64
linux/arm64
linux/arm/7
linux/arm/6
windows/amd64
```

## Key Patterns
- **WAMP Client**: Use `messenger` package for all Crossbar communication
- **Container Ops**: Via `apps` package wrapping Docker API
- **Error Handling**: Use `errdefs` package for typed errors
- **Concurrency**: Use `safe` package for goroutine-safe operations

## Testing
```bash
# Test specific packages
cd src && go test -v reagent/messenger reagent/api

# Tests requiring frpc
make download_frpc && make test-all
```

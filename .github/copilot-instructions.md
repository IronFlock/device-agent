# DeviceManagementAgent - AI Coding Instructions

## Overview
Go-based agent running on IoT devices. Manages containers, handles WAMP communication, provides tunneling via embedded frpc.

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

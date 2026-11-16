# The agent's state file

Status: built on the branch `agent-state`, not released. Unit tests pass.
Open: the VM test with a pulled network cable (plan WP-2.2 in
IronFlock/ironflock-cli).

This file is the contract between the agent and the local tools that read its
state. The first reader is `ironflock status` (IronFlock/ironflock-cli,
`cli/status`). A change of this contract changes both repositories.

## Why

A local tool could not tell whether the agent is connected to IronFlock. The
`connected` marker in `/run/ironflock-device-agent` is written once, at the
first connection, and is not cleared on a disconnect. `last-connected` in the
agent's folder is refreshed only every ten minutes, to spare the SD card. The
state file says what holds now.

## Where and when

- Path: `/run/ironflock-device-agent/state.json`.
- Only on Linux, under the unit `ironflock-device-agent.service`. The legacy
  unit `reagent.service`, the agent in a terminal and Windows write no state
  file.
- `/run/ironflock-device-agent` is the unit's `RuntimeDirectory`. systemd
  creates it empty at each start and removes it at each stop. A file there is
  always from the agent that runs now.
- The agent writes the file at its start, before it connects. It looks at
  its connection and at its update every 5 seconds and writes the file at
  each change. When nothing changes, it writes the file again every minute,
  with a new `updated_at`.
- The agent replaces the file atomically: it writes a temporary file in the
  same folder and renames it over `state.json`. A reader sees the old file or
  the new one, never a part of one.
- Mode `0644`: every user may read it. `ironflock status` runs without root.
- The file holds no secret: no part of the `.flock`, no key, no password.
- A write that fails is logged once. The agent goes on without the file.

## Fields

Example:

```json
{
  "schema": 1,
  "version": "1.6.17",
  "pid": 812,
  "connection": "connected",
  "since": "2026-11-16T09:41:07Z",
  "updated_at": "2026-11-16T10:12:07Z",
  "update": {
    "phase": "probation",
    "new_version": "1.6.17",
    "old_version": "1.6.16",
    "updated_at": "2026-11-16T09:40:55Z"
  }
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `schema` | integer | The version of this format. It is 1. |
| `version` | string | The version of the agent that runs (what `device-agent -version` prints). |
| `pid` | integer | The process ID of the agent. |
| `connection` | string | `connected` while the agent's session to IronFlock is up, else `disconnected`. |
| `since` | string | Since when `connection` holds, in UTC (RFC 3339, seconds). The first `disconnected` holds since the start of the process. |
| `updated_at` | string | When the agent last wrote the file, in UTC (RFC 3339, seconds). |
| `update` | object | The self-update in progress. Not there when there is none. |

`update` holds the fields of `update-state.json` in the agent's folder, in
the same words. That file is root's on some machines (the appliance keeps
`/opt/ironflock` to root), so the state file repeats them.

| Field | Type | Meaning |
| --- | --- | --- |
| `phase` | string | `swapping` (the new binary is moved into place), `probation` (the new version has to connect to prove itself), `suspect` (rolled back; the previous version judges the new one), `struck` (the new version is tried again at the next update) or `rolledback` (the new version failed twice and is blocked until a newer release). |
| `new_version` | string | The version the update brings. |
| `old_version` | string | The version it replaces. |
| `reason` | string | Why `new_version` was rolled back, in the agent's words. Only after a rollback. |
| `updated_at` | string | When the agent last wrote `update-state.json`. |

## How fast a disconnect shows

The agent sends a websocket ping every 5 seconds (`-ppTimeout`, default 5000
milliseconds). When no answer came for 10 seconds, it closes the connection,
and its session is no longer up. The next look, at most 5 seconds later,
writes `disconnected`. A pulled network cable shows after about 15 seconds.
The heartbeat to IronFlock (every 30 seconds, a reconnect after two failures)
is the backstop when the transport does not notice. `connected` shows at most
5 seconds after the session is up again.

## Rules for readers

- Read `schema` first. A reader that does not know the value treats the file
  as absent. The value changes only when a field changes its meaning or goes
  away. A new field leaves it as it is.
- Ignore the fields you do not know.
- No file: the agent does not write one (a release before the state file, the
  legacy layout) or has not started yet. The connection is unknown.
- The file says nothing when the unit does not run: systemd removed the
  folder at the stop. Ask systemd first.
- `updated_at` older than 5 minutes: the agent no longer writes the file (it
  hangs). Treat the connection as unknown.
- The time in `since` and `updated_at` is the device's clock. A clock that
  jumps moves them too.

## In the code

- `src/agentstate`: the file, its fields and the loop that keeps it current.
- `src/service_linux.go` (`runService`): starts the loop under the unit, with
  the session's connection (`lifecycle.Connected`) and the update in progress
  (`selfupdate.Manager.Pending`).

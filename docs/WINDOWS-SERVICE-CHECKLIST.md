# Windows service — manual verification checklist

Run on a real Windows 10/11 machine with Docker Desktop before promoting a
release that touches the service, the self-update or the layout migration.
Automated coverage ends where the SCM, NTFS rename semantics, Defender, and
Docker Desktop's login-scoped engine begin — these must be exercised on
hardware.

Names since agent 1.4.0: service `ironflock-device-agent` ("IronFlock Device
Agent"), folder `%ProgramData%\IronFlock\DeviceAgent`, `device-agent.exe`,
`device-agent.db`, `device-agent.log`. Devices installed before run the
service `reagent` from `%ProgramData%\IronFlock\Reagent` until the migration
(section "Layout migration") moved them.

## Install

1. From an elevated prompt: `device-agent.exe service install -config <flock>`.
   Verify in `services.msc`: `ironflock-device-agent`, delayed auto-start,
   recovery tab shows restart 5s / 30s / 120s with a 1-day reset; event log
   source `ironflock-device-agent` exists; Task Scheduler shows "IronFlock
   Agent Repair" (ONSTART, SYSTEM) running `DeviceAgent\repair.cmd`;
   `icacls %ProgramData%\IronFlock` shows inheritance off, SYSTEM +
   Administrators full, Users read/execute, owner Administrators;
   `icacls %ProgramData%\IronFlock\DeviceAgent` shows SYSTEM + Administrators
   only, with the Users grant on `apps\`.
2. Non-elevated install attempt → clear "elevated prompt" error.
3. Machine with an existing `C:\Users\<x>\reagent\reagent.db` (or a
   `%ProgramData%\IronFlock\Reagent` folder without the `reagent` service) →
   install without `-agentDir` aborts with the two-choice migration message;
   re-run with `-agentDir` pointing at the old dir renames `reagent.db` to
   `device-agent.db` and keeps the existing app data working.
4. Machine with the `reagent` service still installed → `service install`
   refuses and names `service uninstall`.
5. A `%ProgramData%\IronFlock\DeviceAgent` created by a standard user before the
   install → the install refuses (ownership); after removing it the install works.

## Boot / Docker

6. Reboot WITHOUT logging in → service runs, device shows CONFIGURING in
   Studio (WAMP endpoints reachable, remote debugging works). Log in → Docker
   Desktop starts → device goes CONNECTED, apps deploy. Confirm SYSTEM can
   reach `\\.\pipe\docker_engine` (`psexec -s docker version`) and that
   `docker.exe` is on the MACHINE PATH (service sees no user PATH).
7. Compose app full lifecycle: install → RUNNING → reboot → RUNNING again;
   app `/data` contents survive both the reboot and an agent self-update.

## Self-update

8. Studio "update agent": progress events arrive, `device-agent-prev.exe`
   appears, service restarts within ~15s, `device-agent.exe -version` reports
   the new version, `update-state.json` disappears ~2 min later (probation
   passed).
9. Bad update (binary that exits immediately, staged as
   `device-agent-v<ver>.exe` + forced activation): 3 failed starts → rollback
   to prev → marker phase `rolledback` → the same version is NOT re-activated
   on the next update check (blacklist holds, no ping-pong).
10. Vacancy: stop the service, delete `device-agent.exe` (leave
    `device-agent-prev.exe`), reboot → repair task restores the binary and
    starts the service.
11. Defender real-time protection ON during an update: swap succeeds (retry
    logic absorbs scan-handle sharing violations). Note any quarantine events —
    if Defender flags the unsigned exe, code signing moves from follow-up to
    prerequisite.

## Supervision / control

12. `taskkill /f /im device-agent.exe` → SCM restarts after 5s (then 30s/120s
    on repeated kills within a day). `device-agent.exe service stop` → clean
    stop, NO restart. `sc query ironflock-device-agent` matches
    `device-agent.exe service status`.
13. Kill the agent mid compose build → the `docker compose` child processes
    disappear with it (job object).
14. Second instance: with the service running, `device-agent.exe -config ...`
    in a console → refuses with the single-instance message; so does a
    `reagent.exe` from before 1.4.0 (both mutex names are held). FlockFlasher
    "Test Device" on the same machine also refuses (expected, documented).
15. From Studio: `system_restart_agent` (service restarts), `system_reboot`,
    `system_shutdown` (machine reboots/shuts down after ~5s).

## Layout migration (devices installed before 1.4.0)

Start from a 1.3.x service install (`reagent` in `Reagent\`) with one
single-container app that keeps data in an image `VOLUME` (Postgres) and one
compose app with `restart: always`. Let the device update to 1.4.0 from Studio
(the switch `"windows"` absent in `migration.json` → it stays `legacy`, nothing
moves); then `just windows-migration on` or `device-agent.exe migrate-layout
start` from `Reagent\reagent.exe` in an elevated prompt.

16. Gate: the update ran 1.4.0 as `Reagent\reagent.exe`; `stats.layout` says
    `legacy`; `services.msc` still shows `reagent`. With `"windows"` absent
    nothing happens 5 minutes after connecting (log: "not switched on"). A
    `reagent.exe` started from a console (service stopped) reports
    `legacy-console` and never moves, even with the switch on.
17. Happy path: with the switch on, 5 minutes after the next agent start the
    journal appears, `ironflock-device-agent-migrate` runs, `reagent` is gone
    within a few minutes, `ironflock-device-agent` is delayed-auto and
    running from `DeviceAgent\`, `stats.layout` = `ironflock`, the repair task
    runs `DeviceAgent\repair.cmd`, the migrate service and `.migrate` are gone
    after the next agent restart; the Postgres app still has its data; the
    compose app runs from the new folder (`docker inspect` shows
    `DeviceAgent` bind sources); a WSL-engine `DOCKER_HOST` or a proxy from
    the `reagent` service's Environment is on the new service.
18. Rollback: set `IRONFLOCK_MIGRATE_FAULT=health` in the `reagent` service's
    `Environment` (regedit, then `service stop` / `service start`) → the
    journal says `failed_at`, `reagent` runs and reconnects from `Reagent\`
    with the original names (`reagent.db`, `reagent.log`), no `DeviceAgent`
    folder and no `ironflock-device-agent` service remain, `layout_error`
    names the health check. Remove the hook (no restart) and run
    `Reagent\reagent.exe migrate-layout start` → the move goes through.
19. Power cut: `IRONFLOCK_MIGRATE_PAUSE=move-folder`, wait until the journal's
    `step` is 5, then hard-reset the machine (also once with Fast Startup
    shutdown instead of a cold cut) → the boot finishes the move; the apps run
    from the new folder.
20. Docker Desktop down at boot (no auto-login) while a migration is in
    progress, then starting at login → `stop-apps` recorded
    `docker_unavailable`, the move completes, the containers are recreated once
    Docker is up (the `recreate-containers` marker is gone afterwards).
21. An Explorer window open on `Reagent\` → `move-folder` retries for ~2 min,
    then rolls back naming `explorer.exe` in `layout_error`; the next attempt
    (`migrate-layout start`) keeps the folder in place (`strategy` = `in-place`
    with the reason) and succeeds.
22. Docker Desktop Hyper-V backend with a file-sharing list that does not
    include `C:\ProgramData` → `strategy` = `in-place`; with `C:\ProgramData`
    shared → `move`.
23. `service stop` and `service uninstall` while the journal is in progress
    are refused; `migrate-layout status` prints the journal.

## Device terminal (ConPTY)

The host terminal runs PowerShell inside a Windows pseudoconsole, under
LocalSystem — so it is a SYSTEM shell, gated on the DEVELOP privilege exactly
like the Linux/bash one. All of this is unreachable from CI (no ConPTY on a
headless runner, no real console host), so it must be exercised here.

24. Open the device terminal in Studio → a PowerShell prompt appears.
    `whoami` reports `nt authority\system`. `$PSVersionTable` shows the
    expected edition (pwsh 7 wins when installed, otherwise Windows
    PowerShell 5.1). `$env:PATH` contains the machine PATH, and `docker ps`
    works from inside it.
25. Encoding: `"äöü ✓ 日本語 🚀"` and `Get-ChildItem C:\` render correctly —
    no `Ã¤`, no `?`, no `` — both immediately at the prompt and in the
    output of an external command (`cmd /c echo ä`), which is where a
    half-applied code page shows up first.
26. Resize: drag the browser window / toggle the sidebar → the prompt reflows
    and `$Host.UI.RawUI.WindowSize` matches. `vim`-style full-screen output
    (`more`, `Get-Help -Full ... | more`) redraws at the new size.
27. Exit paths: type `exit` → the terminal closes cleanly in the UI (a
    TERMINAL_EOF arrives, not a hang). Reopen → a fresh session starts.
    Then check Task Manager: no orphaned `powershell.exe`/`pwsh.exe` and no
    orphaned `conhost.exe` accumulate across ~5 open/close cycles.
28. Close the browser tab mid-session, then reopen the terminal → new session
    works, and the abandoned shell is gone from Task Manager.
29. Reconnect: block the WAMP endpoint briefly (or `Restart-Service` the
    router side) with the terminal open → after reconnect, typing and resizing
    still reach the shell (the per-terminal topics are re-registered).
30. On a pre-1809 machine, if one is available: the terminal fails with the
    "requires Windows 10 version 1809" message rather than crashing the agent.

## Environment

31. Proxy machine: install with `-proxy http://host:port` → OTA download and
    the wss connection both work under LocalSystem; machine-store corporate
    CA is accepted (Go uses the Windows system store).

## Tunnels (frp) on Windows

32. frpc is downloaded (not embedded) to `<AgentDir>\frpc.exe` from
    `gs://ironflock-downloads/device-agent/frpc/windows/amd64/<ver>/`; confirm it arrives and
    `frpc.exe -version` matches the pinned FRP_VERSION.
33. A full tunnel lifecycle works: publish an app with an HTTP + a TCP port →
    tunnel comes up → reachable from the cloud; `frpc.log` is under the agent
    dir (not `C:\var\log`, and no stray `systemprofile\reagent` folder).
34. Fallback: delete `frpc.exe` at runtime → the agent stays up, apps still
    start (syncPortState no-ops), the tunnel manager re-acquires it (or, if
    blocked, settles to unavailable), `get_agent_metadata` reports
    `tunnelCapable:false`, the app's Remote Access section shows
    `tunnelFeatureUnavailable`, and the device settings header shows a
    "Tunnel disabled" warning badge next to the architecture badge (hover =
    the explanatory tooltip). A healthy device shows no badge.
35. Defender exclusion: after install, `Get-MpPreference | Select ExclusionPath`
    lists `<AgentDir>\frpc.exe`. On a Tamper-Protection / Intune-managed device
    the `Add-MpPreference` warns and is ignored — verify the graceful-degrade
    path (item 34) then covers it, and use the WDAC alternative below.

## Code signing

36. After a signed release: `Get-AuthenticodeSignature device-agent.exe` (and
    `frpc.exe`) shows a valid signature by the IronFlock leaf; the installer
    imported the root (`certutil -store Root` / `-store TrustedPublisher` list
    it); UAC shows "IronFlock" as a verified publisher.
37. On-device pinning: a self-update signed by our leaf verifies; a binary
    signed by any other cert (even one the machine trusts) is rejected once
    enforcement is on (`codesign.Verify` pins to our embedded root).
38. Uninstall symmetry: `device-agent.exe service uninstall` removes the
    Defender exclusion, deletes the imported certs from both stores, and
    removes the cert file (`certutil -store Root` no longer lists IronFlock).

## Enterprise alternative to the Defender exclusion (WDAC)

On fleets that block `Add-MpPreference` (Tamper Protection / Intune), allow the
signed frpc/agent by **publisher** with a WDAC signer rule instead of excluding
a path — this keeps AV scanning other content while trusting our binaries:

- Build a WDAC policy with a Publisher rule for the IronFlock code-signing
  certificate (`New-CIPolicy -Level Publisher -ScanPath <AgentDir>` against the
  signed binaries, or author the rule from the root cert), then deploy it via
  Intune / Group Policy.
- This is the recommended posture for managed devices; the per-device path
  exclusion remains the default for unmanaged installs.

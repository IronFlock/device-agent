# Windows service — manual verification checklist

Run on a real Windows 10/11 machine with Docker Desktop before promoting the
first service-capable release. Automated coverage ends where the SCM, NTFS
rename semantics, Defender, and Docker Desktop's login-scoped engine begin —
these must be exercised on hardware.

## Install

1. From an elevated prompt: `reagent.exe service install -config <flock> -start`.
   Verify in `services.msc`: delayed auto-start, recovery tab shows
   restart 5s / 30s / 120s with a 1-day reset; event log source `reagent`
   exists; Task Scheduler shows "IronFlock Agent Repair" (ONSTART, SYSTEM);
   `icacls %ProgramData%\IronFlock\Reagent` shows SYSTEM + Administrators only,
   with the Users grant on `apps\`.
2. Non-elevated install attempt → clear "elevated prompt" error.
3. Machine with an existing `C:\Users\<x>\reagent\reagent.db` → install without
   `-agentDir` aborts with the two-choice migration message; re-run with
   `-agentDir` pointing at the old dir keeps existing app data working.

## Boot / Docker

4. Reboot WITHOUT logging in → service runs, device shows CONFIGURING in
   Studio (WAMP endpoints reachable, remote debugging works). Log in → Docker
   Desktop starts → device goes CONNECTED, apps deploy. Confirm SYSTEM can
   reach `\\.\pipe\docker_engine` (`psexec -s docker version`) and that
   `docker.exe` is on the MACHINE PATH (service sees no user PATH).
5. Compose app full lifecycle: install → RUNNING → reboot → RUNNING again;
   app `/data` contents survive both the reboot and an agent self-update.

## Self-update

6. Studio "update agent": progress events arrive, `reagent-prev.exe` appears,
   service restarts within ~15s, `reagent.exe -version` reports the new
   version, `update-state.json` disappears ~2 min later (probation passed).
7. Bad update (binary that exits immediately, staged as `reagent-v<ver>.exe`
   + forced activation): 3 failed starts → rollback to prev → marker phase
   `rolledback` → the same version is NOT re-activated on the next update
   check (blacklist holds, no ping-pong).
8. Vacancy: stop the service, delete `reagent.exe` (leave `reagent-prev.exe`),
   reboot → repair task restores the binary and starts the service.
9. Defender real-time protection ON during an update: swap succeeds (retry
   logic absorbs scan-handle sharing violations). Note any quarantine events —
   if Defender flags the unsigned exe, code signing moves from follow-up to
   prerequisite.

## Supervision / control

10. `taskkill /f /im reagent.exe` → SCM restarts after 5s (then 30s/120s on
    repeated kills within a day). `reagent service stop` → clean stop, NO
    restart. `sc query reagent` matches `reagent service status`.
11. Kill the agent mid compose build → the `docker compose` child processes
    disappear with it (job object).
12. Second instance: with the service running, `reagent.exe -config ...` in a
    console → refuses with the single-instance message. FlockFlasher
    "Test Device" on the same machine also refuses (expected, documented).
13. From Studio: `system_restart_agent` (service restarts), `system_reboot`,
    `system_shutdown` (machine reboots/shuts down after ~5s).

## Device terminal (ConPTY)

The host terminal runs PowerShell inside a Windows pseudoconsole, under
LocalSystem — so it is a SYSTEM shell, gated on the DEVELOP privilege exactly
like the Linux/bash one. All of this is unreachable from CI (no ConPTY on a
headless runner, no real console host), so it must be exercised here.

14. Open the device terminal in Studio → a PowerShell prompt appears.
    `whoami` reports `nt authority\system`. `$PSVersionTable` shows the
    expected edition (pwsh 7 wins when installed, otherwise Windows
    PowerShell 5.1). `$env:PATH` contains the machine PATH, and `docker ps`
    works from inside it.
15. Encoding: `"äöü ✓ 日本語 🚀"` and `Get-ChildItem C:\` render correctly —
    no `Ã¤`, no `?`, no `` — both immediately at the prompt and in the
    output of an external command (`cmd /c echo ä`), which is where a
    half-applied code page shows up first.
16. Resize: drag the browser window / toggle the sidebar → the prompt reflows
    and `$Host.UI.RawUI.WindowSize` matches. `vim`-style full-screen output
    (`more`, `Get-Help -Full ... | more`) redraws at the new size.
17. Exit paths: type `exit` → the terminal closes cleanly in the UI (a
    TERMINAL_EOF arrives, not a hang). Reopen → a fresh session starts.
    Then check Task Manager: no orphaned `powershell.exe`/`pwsh.exe` and no
    orphaned `conhost.exe` accumulate across ~5 open/close cycles.
18. Close the browser tab mid-session, then reopen the terminal → new session
    works, and the abandoned shell is gone from Task Manager.
19. Reconnect: block the WAMP endpoint briefly (or `Restart-Service` the
    router side) with the terminal open → after reconnect, typing and resizing
    still reach the shell (the per-terminal topics are re-registered).
20. On a pre-1809 machine, if one is available: the terminal fails with the
    "requires Windows 10 version 1809" message rather than crashing the agent.

## Environment

21. Proxy machine: install with `-proxy http://host:port` → OTA download and
    the wss connection both work under LocalSystem; machine-store corporate
    CA is accepted (Go uses the Windows system store).

## Tunnels (frp) on Windows

22. frpc is downloaded (not embedded) to `<AgentDir>\frpc.exe` from
    `gs://ironflock-downloads/device-agent/frpc/windows/amd64/<ver>/`; confirm it arrives and
    `frpc.exe -version` matches the pinned FRP_VERSION.
23. A full tunnel lifecycle works: publish an app with an HTTP + a TCP port →
    tunnel comes up → reachable from the cloud; `frpc.log` is under the agent
    dir (not `C:\var\log`).
24. Fallback: delete `frpc.exe` at runtime → the agent stays up, apps still
    start (syncPortState no-ops), the tunnel manager re-acquires it (or, if
    blocked, settles to unavailable), `get_agent_metadata` reports
    `tunnelCapable:false`, the app's Remote Access section shows
    `tunnelFeatureUnavailable`, and the device settings header shows a
    "Tunnel disabled" warning badge next to the architecture badge (hover =
    the explanatory tooltip). A healthy device shows no badge.
25. Defender exclusion: after install, `Get-MpPreference | Select ExclusionPath`
    lists `<AgentDir>\frpc.exe`. On a Tamper-Protection / Intune-managed device
    the `Add-MpPreference` warns and is ignored — verify the graceful-degrade
    path (item 24) then covers it, and use the WDAC alternative below.

## Code signing

26. After a signed release: `Get-AuthenticodeSignature reagent.exe` (and
    `frpc.exe`) shows a valid signature by the IronFlock leaf; the installer
    imported the root (`certutil -store Root` / `-store TrustedPublisher` list
    it); UAC shows "IronFlock" as a verified publisher.
27. On-device pinning: a self-update signed by our leaf verifies; a binary
    signed by any other cert (even one the machine trusts) is rejected once
    enforcement is on (`codesign.Verify` pins to our embedded root).
28. Uninstall symmetry: `reagent service uninstall` removes the Defender
    exclusion, deletes the imported certs from both stores, and removes the
    cert file (`certutil -store Root` no longer lists IronFlock).

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

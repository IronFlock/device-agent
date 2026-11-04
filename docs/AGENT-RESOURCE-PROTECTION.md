# Agent resource protection

Status: released in agent 1.5.5 on 4 November 2026. Unit tests pass. On the
Windows field device the priorities are as planned (Tests, step 6), and a
reconnect no longer adds a log tail (Measurements, "Windows field device on
1.5.5"). Open: the VM and load tests (steps 4 and 5).

## Goal

The agent must stay responsive when apps use all CPU or memory. It stays
connected to IronFlock, answers commands, keeps tunnels open and opens a
terminal. The connection is the only way to repair a device remotely.

## Where we are

**CPU is covered on Linux.** The unit that `service install` and
`migrate-layout` write (`src/linuxservice/linuxservice.go`) sets
`CPUWeight=2048` and `Nice=-16`. The weight does the work: the kernel splits
CPU time between cgroups first, and every app container has its own cgroup with
the default weight 100. Nice only ranks processes inside the same cgroup.

**Memory is not protected.** The agent has no OOM score adjustment and no
reclaim protection. App containers have no default memory limit. When an app
fills RAM, the kernel reclaims cached file pages, including the agent's own
program code (17 MB of the 47 MB binary are resident, see Measurements). The
agent then stalls on page faults, its WAMP session times out and the device
shows offline. If memory stays short, the OOM killer can pick the agent.

**The terminal shell inherits the agent's priority.** `src/terminal/pty_unix.go`
starts bash as a child of the agent, so it runs at nice -16 in the agent's
cgroup. A heavy command typed into the device terminal outranks every app and
competes with the agent for the agent's own CPU share. With the memory
protection below, it would also use up the agent's protected memory.

**Existing devices do not get unit changes.** Only `service install` and
`migrate-layout` write the unit. A self-update does not rewrite it. Devices on
the legacy `reagent.service` run a unit without any of these settings.

**Windows sets nothing.** The service runs at normal priority.

## Measurements

### Local, 4 November 2026

Agent 1.4.8 (linux/arm64) in the local development stack: a Docker container
with its own cgroup v2, kernel 7.0, 5 hours after start, no apps installed. The
container's cgroup holds the agent and all its children, as the unit's cgroup
does on a device.

| Process | Resident | Anonymous (heap, stacks) | File (program code) | Peak resident since start |
| --- | --- | --- | --- | --- |
| device-agent | 29.7 MB | 12.5 MB | 17.2 MB | 30.6 MB |
| frpc | 24.8 MB | 11.8 MB | 13.0 MB | 24.8 MB |

The cgroup as a whole: `memory.current` 155 MB, `anon` 29 MB, `file_mapped`
33 MB. The other ~90 MB are inactive page cache (the log file, the database),
which the kernel reclaims first and which needs no protection. `memory.peak`
since start is 230 MB, page cache included.

**Every running compose app adds two processes to the agent's cgroup.** For
each app the agent keeps a live log tail, `docker compose -f … logs -f`
(`src/container/compose.go`, started for every running app by
`src/logging/applog.go` and `src/apps/run_app.go`), so that log history keeps
accumulating whether or not anyone is watching. Each tail is a `docker` CLI
process plus the `docker-compose` plugin it runs. Two tails measured in the same
container:

| Process | Resident | Anonymous | File |
| --- | --- | --- | --- |
| docker (tail 1) | 33.4 MB | 13.5 MB | 19.9 MB |
| docker (tail 2) | 37.7 MB | 17.6 MB | 20.1 MB |
| docker-compose (tail 1) | 33.1 MB | 12.4 MB | 20.7 MB |
| docker-compose (tail 2) | 34.0 MB | 13.5 MB | 20.5 MB |

The cgroup grew by 58 MB for the two. So each running compose app costs about
28 MB of anonymous memory, and the CLI and plugin code (about 40 MB) is counted
once, shared by all tails. With five compose apps, the cgroup's working set grows
from about 55 MB (agent + frpc) to about 235 MB. Single-container apps are read
through the Docker API inside the agent and add no processes.

Not measured locally: the agent during an app install with an image pull, with
many log lines per second, with an open terminal. The peak values from field
devices (Appendix) cover those.

### Field, 4 November 2026

**Linux device.** Agent 1.5.4 under `ironflock-device-agent.service`, 4 hours
after start. Debian 13, kernel 6.12, systemd 257, cgroup v2 mounted with
`memory_recursiveprot`, x86_64, 3.9 GB RAM, 3 GB swap (about 116 MB in use). An
appliance host: 24 running containers, one compose app plus the appliance
platform's own compose project.

| Process | Resident | Anonymous | File | Peak resident |
| --- | --- | --- | --- | --- |
| device-agent | 33.0 MB | 14.6 MB | 18.4 MB | 33.6 MB |
| frpc | 25.1 MB | 11.5 MB | 13.6 MB | 25.2 MB |
| docker (log tail) | 30.5 MB | 11.4 MB | 19.1 MB | 30.5 MB |
| docker-compose (log tail) | 33.8 MB | 12.2 MB | 21.6 MB | 33.8 MB |
| bash, sudo (terminal session) | 16.6 MB | 3.6 MB | 13.1 MB | |

The cgroup: `memory.current` 63 MB, `memory.peak` 66 MB, `anon` 53 MB,
`file_mapped` 0.8 MB.

- Agent + frpc peak at 58.8 MB, close to the local 55 MB. One log tail has
  23.6 MB of anonymous memory, close to the local 28 MB.
- The terminal session that ran the measurement sits in the agent's cgroup, as
  described under "Where we are" (change 3).
- **The program code is not charged to the agent's cgroup.** The processes have
  about 80 MB of file pages resident, but the cgroup holds only 0.8 MB of mapped
  file pages. The kernel charges a cached file page to the cgroup that first
  loaded it, and the charge stays there. Each restart of the unit gets a new
  cgroup, so the code that an earlier run loaded stays charged to that run's
  removed cgroup: the agent binary the self-update wrote, frpc unpacked at an
  earlier start. `MemoryMin=` on the unit would therefore protect mostly heap
  and stacks, not the code. See 2.2 for the consequence.
- The one compose app has exactly one log tail. The platform's own compose
  project is not an app and has none.

**Windows device.** The agent's process tree 17 hours after start, in MB
(Windows reports the working set and the private bytes):

| Process | Count | Working set each | Peak working set each | Private each |
| --- | --- | --- | --- | --- |
| device-agent | 1 | 36.0 | 42.9 | 66.1 |
| frpc | 1 | 45.4 | 62.1 | 71.5 |
| docker (log tail) | 8 | 19.5–20.2 | 21.1–22.1 | 54.2–54.8 |
| docker-compose (log tail) | 8 | 14.8–30.1 | 83.5–86.1 | 57.8–59.8 |
| conhost (one per console process the service starts) | 9 | 10.6–10.7 | 11.4–11.5 | 1.7 |

- The 8 log tails take about 420 MB of working set and about 920 MB of private
  (committed) memory, five times the working set of agent + frpc (81 MB).
- **Duplicate tails, confirmed.** The device runs 2 compose apps, and each has
  4 tails (Appendix, "Checking the log tails"). They started with the agent
  (17.3 hours ago) and then again for both apps at three later times (13.2,
  11.4 and 10.1 hours ago), without the old ones being stopped. The 6 left-over
  tails hold about 320 MB of working set and 690 MB of committed memory after
  17 hours, and every further event adds two more.
- **Cause, from the code.** `Agent.OnConnect` (`src/agent.go`) runs after every
  WAMP connection, the reconnects included, and calls
  `LogManager.ReviveDeadLogs` (`src/logging/applog.go`). That function skips a
  running app only when its log entry holds a Docker API stream
  (`entry.Stream != nil`). A compose app's entry is made by `StreamLogsChannel`
  and holds a channel instead, so it is never skipped: every reconnect starts
  one more `docker compose logs -f` for every running compose app.
  `Compose.LogStream` (`src/container/compose.go`) used to kill the earlier
  tail first; that code is commented out. The Docker daemon recovery
  (`src/apps/daemon_recovery.go`) calls `ReviveDeadLogs` too, but it is not
  what happened here: the log's last "container event stream lost" is from
  30 October, the extra tails are from 4 November. Linux runs the same code and
  leaks the same way on every reconnect; the Linux device above had one tail
  after 4 hours without a reconnect.

### Windows field device on 1.5.5, 4 November 2026

The same device, 20 minutes after the update from 1.5.3 to 1.5.5 (Appendix,
Windows and "Checking the log tails"), in MB:

| Process | Count | Priority | Working set each | Peak working set each | Private each |
| --- | --- | --- | --- | --- | --- |
| device-agent | 1 | AboveNormal | 34.8 | 39.8 | 62.0 |
| frpc | 1 | AboveNormal | 28.8 | 62.1 | 52.6 |
| docker (log tail) | 2 | Normal | 20.4–20.6 | 21.2–21.6 | 53.9–54.5 |
| docker-compose (log tail) | 2 | Normal | 46.8 | 83.8–84.1 | 58.3–58.8 |
| conhost (frpc, log tails) | 3 | Normal | 11.4 | 11.4 | 1.7 |
| powershell, conhost (the terminal session that ran the measurement) | 1 + 1 | Normal | 103.9, 7.5 | 150.1, 7.5 | 81.6, 1.4 |
| vmmemWSL (the WSL VM with the apps; not started by the agent) | 1 | Normal | 2928.6 | 4418.5 | 4184.2 |

- **The priorities are as planned** (Tests, step 6): the agent and frpc run
  above normal; everything else the agent starts runs at normal priority,
  the terminal shell included. The VM that runs the apps is at normal
  priority, so on the host the agent and frpc come first when the apps load
  the CPU. The log has no `could not raise the agent's priority class`.
- **One tail per compose app**: 2 apps, 2 tails, both started with the
  agent. The update ended the 8 tails of 1.5.3: the job object stopped them
  with the old agent. The tree without the terminal session holds 232 MB of
  working set and 345 MB of private memory; 1.5.3 held about 600 MB and
  1,070 MB after 17 hours. That comparison favours 1.5.5: 1.5.3 also had 2
  tails right after its start, and the 6 extra ones came from reconnects.
- **A reconnect leaves the tails alone.** The agent lost its connection at
  20:34:49 and was re-initialized at 20:34:51. 35 seconds later the device
  still had the same 2 tails, started with the agent, and no new one. Under
  1.5.3 this device reconnected at 01:38, 03:29 and 04:46 on 4 November, and
  each time both apps got one more tail within the same seconds. The terminal
  session that ran the measurement survived the reconnect.
- A young docker-compose process holds 46.8 MB of working set; after 17
  hours they held 14.8–30.1 MB, because Windows trims idle processes. Private
  memory (about 58 MB) does not shrink, so a tail costs about 115 MB of
  committed memory either way.

### Consequences for this plan

- **The protection goes on the slice, not the unit** (2.2). With the cgroup
  mount option `memory_recursiveprot`, the slice's protection covers all its
  child cgroups, including the removed cgroups of earlier runs that hold the
  code.
- **`MemoryMin=` is sized for the agent and frpc, not the log tails.** Covering
  five compose apps would take about 235 MB from every device, about a quarter
  of a 1 GB device. The protection applies to the slice as a whole. Below `<N>`
  nothing in it is reclaimed; above `<N>` the kernel takes the coldest pages
  first (old log cache, tail code) before the agent's code, which it runs all
  the time.
- **The live log tail through the Docker API** would save about 24–28 MB per
  running compose app on Linux and about 50 MB of working set and 115 MB of
  committed memory per app on Windows. The agent's memory would no longer grow
  with the number of apps. The query path already reads compose logs this way
  (`src/logging/query.go`). This is a separate effort (see Scope).
- **The duplicate tails are a leak that grows with every reconnect** and, on
  Windows, threatens exactly what this plan protects: a device on a poor
  connection leaks fastest. It is fixed first, on this branch (see Scope).

## Scope

In this plan:

1. An OOM score for the agent (Linux).
2. Memory protection through its own slice (Linux).
3. The terminal shell back at normal priority (Linux).
4. Delivery to existing devices through a drop-in that the agent writes at
   start.
5. Process priority on Windows.

Not in this plan (separate efforts, if wanted):

- **Capping apps**, e.g. all app containers in one slice with `MemoryHigh=` set
  to RAM minus a reserve. It is the only hard guarantee, but it changes how
  customer apps behave.
- **Disk I/O priority.** `IOWeight=` needs BFQ or the io.cost controller, which
  SD and eMMC devices rarely use.
- **Docker daemon priority.** App commands still go through dockerd. This plan
  keeps the agent online, not every app command fast.
- **Windows memory.** Apps run inside the WSL VM, which WSL caps at half the
  host's RAM by default, so they cannot exhaust the host.
- **The live log tail through the Docker API** instead of two CLI processes per
  compose app (see Measurements). It saves memory on every device, not only
  under pressure, and is the most useful follow-up, above all on Windows.
- **Duplicate log tails** (see Measurements): fixed on this branch on
  4 November 2026, ahead of the rest. `ReviveDeadLogs` treats a live compose
  tail like a live Docker API stream; `StreamComposeTail` stops an app's earlier
  tail before it follows a new one; a tail is its own process group (Linux) or
  is stopped with `taskkill /T` (Windows), so the compose plugin ends with the
  CLI; a tail that ends is reaped instead of staying a zombie. Tests:
  `src/container/compose_tail_test.go`, `src/logging/applog_tail_test.go`, each
  run against the old code as a negative control.

## Changes

### 1. OOM score (Linux)

At start, the agent writes `-900` to `/proc/self/oom_score_adj` when it runs
under one of its units (see 2.3). This works on every install kind and takes
effect at once, with no unit change.

Not `-1000`: that exempts the process completely, so an agent that leaks memory
would make the kernel kill everything else. At `-900` the kernel kills a leaking
agent last, and systemd restarts it.

Child processes inherit the value. That is right for frpc (the tunnel is part
of the agent's job) and harmless for the short `docker` CLI calls. Change 3
resets the terminal shell.

### 2. Own slice with memory protection (Linux)

#### 2.1 Why a slice

`MemoryMin=` only protects a unit if every ancestor slice up to the root also
carries protection (see systemd.resource-control). The agent runs in
`system.slice`, which has none. Instead of changing the host's `system.slice`,
the agent moves into its own top-level slice, `ironflock.slice`. Its parent is
the root slice, which is exempt from the rule.

#### 2.2 Files

`/etc/systemd/system/ironflock.slice`:

```ini
[Unit]
Description=IronFlock device agent

[Slice]
CPUWeight=2048
MemoryMin=<N>M
```

`/etc/systemd/system/<unit>.d/10-ironflock-resources.conf`:

```ini
# Written by the IronFlock device agent at every start. Local changes go
# into a drop-in that sorts after this one.
[Service]
Slice=ironflock.slice
CPUWeight=2048
Nice=-16
ManagedOOMPreference=avoid
```

- **The slice carries the CPU weight now.** The agent is alone in its slice, so
  the unit's own weight no longer counts. With 2048 against `system.slice` (100)
  and `user.slice` (100), the agent gets what it asks for under contention. It
  only uses what it needs.
- **`MemoryMin=` only on the slice.** The field measurement showed that the
  agent's code is charged to the removed cgroups of earlier runs of the unit,
  not to the running one. These cgroups are children of the slice too. With the
  cgroup mount option `memory_recursiveprot` (kernel 5.7 and later; systemd sets
  it where the kernel has it), a slice's protection is shared among all its
  children by their usage, the removed ones included. A `MemoryMin=` on the unit
  would claim the protection for the running cgroup alone.
- **Without `memory_recursiveprot`**, children get none of the slice's
  protection. The agent reads the mount options of `/sys/fs/cgroup` and then
  also writes `MemoryMin=<N>M` into the drop-in, which protects at least heap
  and stacks.
- **Code loaded before the move** stays charged to a removed cgroup in
  `system.slice` and is not protected. It moves into the slice when the kernel
  reclaims it and the agent loads it again, or with the next update, whose
  binary the agent writes from inside the slice.
- **`Nice=-16` and `CPUWeight=2048` repeat the unit's lines**, so legacy units
  get them too. Nice still ranks the agent above its own children, and it
  raises the agent's I/O priority level on BFQ.
- **`ManagedOOMPreference=avoid`**, because desktop Ubuntu runs systemd-oomd,
  which kills whole cgroups under memory or swap pressure.
- **The unit template stays as it is.** From now on, resource settings live in
  the drop-in.

#### 2.3 Which unit

The agent reads its unit from `/proc/self/cgroup`: the `0::` line on cgroup v2,
the `name=systemd` line on v1. It acts only for `ironflock-device-agent.service`
and the legacy `reagent.service`. Anything else (a console run, a container,
another supervisor) gets no files and no OOM score.

#### 2.4 When the files are written

One function in `linuxservice` renders and writes both files. It is called:

- by the agent at every start, before `runAgent`, for supervised and legacy runs
  alike (`src/main.go`);
- by `service install`, before the first start;
- by `migrate-layout` when it prepares the new unit, before it copies the legacy
  drop-ins, so that a copy from `reagent.service.d` does not shadow the current
  file.

Rules:

- Each file is written only when its content differs, atomically
  (`WriteFileAtomic`), the slice before the drop-in. `daemon-reload` runs only
  after a change, so a device reloads once per changed version, not at every
  start.
- At agent start, the files and the reload run in the background, and the
  reload has a one-minute timeout: the start never waits for them (they take
  effect at the next start anyway). Only the OOM score is set before the start
  goes on.
- A failure (a read-only `/etc` on some OS images, no systemd) is a warning,
  never fatal, also in `service install` and `migrate-layout`. The OOM score
  from change 1 still applies.
- The agent never restarts for this. Slice and memory protection take effect at
  the next start of the unit, i.e. the next update or reboot. An extra restart
  right after an update would land inside the update's probation window.

#### 2.5 Older systems

- **cgroup v1** (e.g. Ubuntu 20.04) has no `memory.min`, and systemd ignores
  the setting. The CPU weight and the OOM score still apply.
- **systemd before 240** does not know `MemoryMin=`, and before 248 it does not
  know `ManagedOOMPreference=`. It logs a warning, ignores the line and still
  starts the unit.
- **Kernels before 5.7** have no `memory_recursiveprot`. The unit then carries
  `MemoryMin=` itself (2.2), and code charged to removed cgroups is not
  protected.

### 3. Terminal shell at normal priority (Linux)

`startHostShell` in `src/terminal/pty_unix.go`:

- **With systemd**, start the shell through
  `systemd-run --scope --quiet --collect --slice=system.slice -- bash`. The
  shell gets its own scope in `system.slice`. It no longer shares the agent's
  CPU share and does not use the agent's protected memory. `systemd-run
  --scope` execs bash in place, so the PID the agent holds is still the
  shell's. Whether this works is probed once per agent run with `-- true`
  (no systemd, no right to talk to PID 1, systemd before 236); when it does
  not, the shell starts as before.
- **Always**, right after the start, set the shell's nice to 0 (`setpriority`)
  and its `oom_score_adj` to 0. Processes that the shell's rc files start in
  these first microseconds keep the agent's values. That is acceptable.
- **Without systemd**, start the shell as today, plus the reset.
- Closing the terminal still kills the shell, as today. When the agent restarts,
  the pty master closes and the shell gets SIGHUP, as today.

The shell inside a container (`src/terminal/docker_term.go`) runs in the
container's cgroup and is not affected.

### 4. Windows

- At start, the service raises its own priority class to
  `ABOVE_NORMAL_PRIORITY_CLASS`. Not `HIGH_PRIORITY_CLASS`, which can starve
  Windows' own services.
- Child processes start at normal priority by default (CreateProcess), so the
  ConPTY terminal shell needs no change.
- frpc starts with `ABOVE_NORMAL_PRIORITY_CLASS` in its creation flags, so the
  tunnel keeps pace with the agent, as on Linux.

## Decisions

All three taken as proposed on 4 November 2026; the implementation uses
`<N>` = 96 MiB (`MemoryMinMiB` in `src/linuxservice/resources.go`).

1. **`<N>` for `MemoryMin=`.** Proposal: one fixed value for all devices, the
   peak of agent + frpc × 1.5, rounded up to 16 MiB. The highest peak so far is
   the Linux field device's 58.8 MB (code included); × 1.5 ≈ 84 MiB, so
   **96 MiB**. Apps lose that much memory on every device: about 2% of a 4 GB
   device, about 9% of a 1 GB device.
2. **The slice's CPU weight.** Proposal: 2048, the number the unit uses today.
3. **The terminal in its own scope.** Proposal: yes.

## Tests

1. **Measure the agent.** Done locally (Measurements). On field devices, run
   the commands in the Appendix on Linux and Windows devices with real apps:
   once in normal operation and once right after an app install or update.
   The peak values (`VmHWM`, `PeakWorkingSet64`) cover everything since the
   agent started. `<N>` is set from the highest agent + frpc peak.
2. **Unit tests:** rendering of both files; unit detection from
   `/proc/self/cgroup` samples (v1, v2, hybrid, console run, container); writes
   only on change; no `daemon-reload` without a change; `migrate-layout` writes
   the new drop-in before it copies the legacy ones. Each with a negative
   control.
3. **Terminal test as root:** the shell has nice 0 and `oom_score_adj` 0, and
   its cgroup is a `run-*.scope`, not the agent's.
4. **End-to-end on VMs** (fresh install, migrated device, legacy unit, the
   oldest systemd in the matrix): after the second agent start, the agent's
   cgroup is under `ironflock.slice`, the slice's `memory.min` is set,
   `oom_score_adj` is -900, and the unit starts without errors. After an agent
   update, the slice's `file_mapped` holds the agent's code (the unit's cgroup
   alone did not, see Measurements).
5. **Load test on a 1–2 GB VM:** an app allocates memory until the OOM killer
   runs, then an app burns all CPU. The device must stay online, answer a
   command and open a terminal, and the OOM killer must pick the app. Negative
   control: the same test against the current release shows the stall or the
   kill.
6. **Windows:** the agent's priority class is `AboveNormal`, frpc's is
   `AboveNormal`, and a terminal shell's is `Normal`. Passed on the Windows
   field device on 1.5.5 (Measurements).

## Rollout

- This ships in a normal agent release. Devices get the OOM score and the
  Windows priority when the release starts, and the slice and memory protection
  at the start after that.
- Release note line under Changed: the agent protects its memory and CPU share,
  so devices stay reachable when apps use up the device's resources.

## Risks

- **A wrong line in the drop-in could stop the agent from starting.** systemd
  ignores unknown or invalid values with a warning instead of failing the unit,
  the slice file is written before the drop-in, and test 4 covers the oldest
  systemd.
- **Apps get `<N>` MiB less before the OOM killer acts.** That is the trade.
- **An agent that leaks past `<N>`** loses protection for the excess and can
  still be killed and restarted.
- **A runaway agent can take most of the CPU under contention.** That is already
  true today.

## Appendix: measuring the agent on a device

Both scripts only read. They print the agent's version and running time, the
host's memory, and the memory of the agent and every process it started.

### Linux

In the device terminal or an SSH session (drop `sudo` where it is missing; the
device terminal already runs as root):

```bash
sudo bash <<'EOF'
pid=""; unit=""
for u in ironflock-device-agent reagent; do
  p=$(systemctl show -p MainPID --value "$u" 2>/dev/null)
  if [ -n "$p" ] && [ "$p" != 0 ]; then pid=$p; unit=$u; break; fi
done
[ -z "$pid" ] && pid=$(pgrep -o -x device-agent || pgrep -o -f 'Reagent|reagent-latest')
[ -z "$pid" ] && { echo "no agent process found"; exit 1; }
exe=$(readlink -f /proc/$pid/exe)
echo "unit: ${unit:-none}  pid: $pid  version: $($exe -version 2>/dev/null)  running: $(ps -o etime= -p $pid | tr -d ' ')"
echo "host: $(uname -m), kernel $(uname -r), $(systemctl --version | head -1), cgroup fs $(stat -fc %T /sys/fs/cgroup)"
grep -E '^(MemTotal|MemAvailable|SwapTotal|SwapFree):' /proc/meminfo | tr -s ' ' | tr '\n' ' '; echo
echo "binary: $exe, $(stat -c %s "$exe") bytes"
cg=$(grep -E '^0::|name=systemd:' /proc/$pid/cgroup | head -1)
echo "cgroup: $cg"
echo "cgroup2 mount options: $(awk '$2=="/sys/fs/cgroup" && $3=="cgroup2" {print $4}' /proc/mounts)"
echo "processes in the agent's cgroup:"
for d in /proc/[0-9]*; do
  grep -qxF "$cg" $d/cgroup 2>/dev/null || continue
  printf '  %-16s pid %-7s %s\n' "$(cat $d/comm)" "${d#/proc/}" "$(grep -E '^(VmHWM|VmRSS|RssAnon|RssFile|VmSwap):' $d/status | tr -s ' \t' ' ' | tr '\n' ' ')"
done
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  m=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/$pid/cgroup)
  for f in memory.current memory.peak memory.min; do [ -f $m/$f ] && echo "$f: $(cat $m/$f)"; done
  grep -E '^(anon|file|file_mapped|active_file|inactive_file|shmem|kernel) ' $m/memory.stat | tr '\n' ' '; echo
  s=$(dirname $m)
  [ -f $s/memory.stat ] && echo "parent $s: memory.current $(cat $s/memory.current), memory.min $(cat $s/memory.min), $(grep '^file_mapped ' $s/memory.stat)"
else
  m=/sys/fs/cgroup/memory$(sed -n 's/^[0-9]*:memory://p' /proc/$pid/cgroup)
  echo "memory cgroup (v1): $m"
  for f in memory.usage_in_bytes memory.max_usage_in_bytes; do [ -f $m/$f ] && echo "$f: $(cat $m/$f)"; done
  [ -f $m/memory.stat ] && grep -E '^(rss|cache|mapped_file|active_file|inactive_file|shmem) ' $m/memory.stat | tr '\n' ' '; echo
fi
echo "running containers: $(docker ps -q 2>/dev/null | wc -l), compose apps: $(docker compose ls -q 2>/dev/null | wc -l)"
echo "OOM kills in the journal: $(journalctl _TRANSPORT=kernel --no-pager 2>/dev/null | grep -c 'Killed process')"
journalctl _TRANSPORT=kernel --no-pager 2>/dev/null | grep 'Killed process' | tail -3 | sed -E 's/.*Killed process [0-9]+ \(([^)]*)\).*/  killed: \1/'
EOF
```

On cgroup v1 without memory accounting, the memory cgroup path is `/`, and the
cgroup totals are then the whole machine's; the per-process lines still apply.

### Windows

In a PowerShell window opened as Administrator, or in the device's terminal
in Studio (the terminal's own shell then shows up, at normal priority):

```powershell
$svc = Get-CimInstance Win32_Service | Where-Object { $_.Name -in 'ironflock-device-agent','reagent' -and $_.ProcessId -ne 0 } | Select-Object -First 1
$exe = $svc.PathName -replace '^"?([^"]+?\.exe).*', '$1'
$all = Get-CimInstance Win32_Process
$agent = $all | Where-Object ProcessId -eq $svc.ProcessId
$ids = @($svc.ProcessId)
do { $more = @($all | Where-Object { $ids -contains $_.ParentProcessId -and $ids -notcontains $_.ProcessId } | ForEach-Object ProcessId); $ids += $more } while ($more.Count -gt 0)
$base = @{4='Idle';6='BelowNormal';8='Normal';10='AboveNormal';13='High';24='RealTime'}
"version: $(& $exe -version)  agent pid: $($svc.ProcessId)  started: $($agent.CreationDate)  now: $(Get-Date)  RAM: $([math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory/1MB)) MB"
$all | Where-Object { $ids -contains $_.ProcessId -or $_.Name -like 'vmmem*' } | Sort-Object CreationDate | ForEach-Object { $p = Get-Process -Id $_.ProcessId -ErrorAction SilentlyContinue; [pscustomobject]@{ Name = $_.Name; Id = $_.ProcessId; Parent = $_.ParentProcessId; Class = $p.PriorityClass; Base = "$($base[[int]$_.Priority]) $($_.Priority)"; 'WS MB' = [math]::Round($p.WorkingSet64/1MB,1); 'Peak WS MB' = [math]::Round($p.PeakWorkingSet64/1MB,1); 'Private MB' = [math]::Round($p.PrivateMemorySize64/1MB,1); Started = $_.CreationDate.ToString('dd.MM HH:mm:ss') } } | Format-Table -AutoSize
```

`vmmem` / `vmmemWSL` is the WSL VM, which holds the apps. `Class` comes from
the process handle and stays empty where access is denied; `Base` is the base
priority Windows reports for every process.

### Checking the log tails

Every running compose app should have exactly one log tail. These list the tails
by the compose file they follow; a count above 1 is a duplicate, and an app
without a line has no tail.

Linux:

```bash
sudo docker compose ls
sudo ps -o pid,etime,args -C docker-compose
```

Windows, in the same window, right after the commands above (it uses `$ids`;
`FromThisAgent` below `Count` means a tail outlived an earlier agent):

```powershell
Get-CimInstance Win32_Process -Filter "Name='docker-compose.exe'" | Group-Object CommandLine | ForEach-Object { [pscustomobject]@{ Count = $_.Count; FromThisAgent = @($_.Group | Where-Object { $ids -contains $_.ProcessId }).Count; Started = ($_.Group | ForEach-Object { $_.CreationDate.ToString('dd.MM HH:mm:ss') }) -join ', '; Tail = $_.Name } } | Format-Table -AutoSize -Wrap
```

Duplicates come from reconnects. The tails' start times should match these
lines in the agent log (Linux: `/var/log/ironflock/device-agent.log`; Windows:
`device-agent.log` under `%ProgramData%\IronFlock`):

```bash
sudo grep -E 'connection lost|reconnect' /var/log/ironflock/device-agent.log | tail -20
```

```powershell
Get-ChildItem C:\ProgramData\IronFlock -Recurse -Filter device-agent.log | Select-String -Pattern 'connection lost|reconnect' | Select-Object -Last 20 | ForEach-Object Line
```

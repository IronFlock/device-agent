# The Windows agent's proxy: store, start, `service configure`

This is the contract between the Windows device agent (`device-agent.exe`, this
repository) and its callers, above all the `ironflock` CLI (`ironflock agent
install|configure --http-proxy --https-proxy --no-proxy --proxy-password-file
--remove-proxy`, `ironflock status`, `ironflock doctor`). It says where the
agent keeps its proxy, what the agent does with it at its start, and the
command lines a caller uses to change and read it. Linux and macOS are not
affected by anything here.

Status: first agent with this contract: the release after 1.6.19 (callers
never compare versions: they use the capability probe below).

## OPEN

Nothing in the interface is open. Decisions the owner may still turn around
(each is built as described here):

- O1. `service configure` refuses to restart the service when it runs inside
  the agent's own process tree (the device terminal in Studio): the stop would
  end the command before the start. Exit 3, nothing changed; `-no-restart`
  is the way to write the settings from there.
- O2. While the agent's own move (below) is pending, `service configure` also
  writes the new values into the service's registry environment, where every
  user can read them, so that a rollback to the old agent keeps working with
  the new proxy. The alternative is to drop them there and accept that a
  rollback in that window comes up without a proxy.
- O3. The non-secret fact "a proxy is configured" is kept in the registry
  (`ProxyConfigured`, below), readable by every user.

## The store

| | |
|---|---|
| Path | `<agentDir>\proxy.env`; `<agentDir>` is the `-agentDir` in the service's ImagePath (`HKLM\SYSTEM\CurrentControlSet\Services\ironflock-device-agent`, value `ImagePath`), by default `%ProgramData%\IronFlock\DeviceAgent` |
| Format | UTF-8 text, one `NAME=value` per line, `#` comment lines and blank lines allowed; the same file and format as before (`service install` has written it since 0.21.11) |
| Names | `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` (read case-insensitively; other names are ignored). Written in this order, upper case, values unquoted. On read a value may be enclosed in `"…"` or `'…'`, which is taken off |
| Values | `HTTP_PROXY` and `HTTPS_PROXY`: a proxy URL (`http`, `https`, `socks5`, `socks5h`), each on its own, so the two can differ; a user and password as `http://user:password@host:port` with the user and password percent-encoded as Go's `url.UserPassword` writes them; the empty password as `http://user:@host:port`. `NO_PROXY`: the operator's own entries only (what `-no-proxy` gave), comma-separated host names, domains, addresses, CIDRs, kept as given; the agent adds what the device reaches directly at every start (see "The agent's NO_PROXY"). A store written by an agent up to 1.6.19 holds that agent's whole list here, which then counts as the operator's: nothing is lost |
| Configured | the device has a proxy when `HTTP_PROXY` or `HTTPS_PROXY` is not empty. A missing file, or one with both empty, means: no proxy |
| ACL | owner SYSTEM or Administrators; inheritance off (protected DACL); exactly two entries: SYSTEM full control, Administrators full control. SDDL `D:P(A;;FA;;;SY)(A;;FA;;;BA)`. A standard user cannot read it |
| Written | atomically: a temporary file `proxy.env.tmp` created with that ACL, flushed, then renamed over `proxy.env` (`MoveFileEx` replace + write-through); the ACL is checked after the rename and repaired if needed |
| Checked | the ACL is checked, and repaired when it differs, each time the file is written and at every start of the agent service |
| Kept | `service uninstall` keeps the file; a later `service install` uses it (it wins over `-proxy` unless `-force-proxy`), as before |

An administrator may still edit the file by hand; the agent takes the edit at
its next start (`device-agent.exe service stop` and `start`, or a restart
from Studio).

`proxy.env.old`: when the agent's move (below) finds a `proxy.env` whose
values differ from those in effect (the service's registry environment), it
keeps the file's earlier content here once, with the same ACL, before it
writes the values in effect into `proxy.env`. Nothing reads it.

Non-secret fact for every user: `HKLM\SYSTEM\CurrentControlSet\Services\ironflock-device-agent\Parameters`,
value `ProxyConfigured` (REG_DWORD): 1 the agent has a proxy, 0 it has none.
Written by `service install`, `service configure` and the agent at its start.
Missing: not known yet (an agent that has not started since its update). The
service key is readable by every user; the value holds no URL.

The service's registry environment (`Environment`, REG_MULTI_SZ, of the same
key) holds no proxy value any more: only what others put there, such as a
WSL engine's `DOCKER_HOST` (below) or `AGENT_UPDATE_ALLOW_UNSIGNED`.

## What the agent does at its start

Only when it runs as the Windows service; a console run of `device-agent.exe`
changes nothing. Before any network use (the update check, the connection to
IronFlock, the tunnels, the app containers):

1. Read the store; check its ACL and repair it.
2. Read the proxy values of the service's registry environment
   (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, any case).
3. Decide what is in effect:
   - the registry environment has proxy values (a device installed by an
     older agent, or the move below not finished): those are in effect, as
     they were for the older agent. A missing store is created from them; a
     store with other values is kept as `proxy.env.old` and written anew
     with them;
   - otherwise the store's values.
4. Set the agent's own process environment from the values in effect:
   `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` set (an empty value removes the
   variable); `NO_PROXY` is the list computed from the stored one (see "The
   agent's NO_PROXY"). Windows' environment is case-insensitive, so `http_proxy` and
   the others read the same values. Without a proxy in effect the process
   environment is left as Windows gave it (a machine-wide proxy variable
   still applies, as before).
5. Write `ProxyConfigured`.

The move of an existing install (no reinstall, no operator): after the agent's
self-update the new agent runs with the old registry environment. Step 3
creates or aligns the store and step 1 tightens its ACL at once. The proxy
values are taken out of the registry environment (every other value stays)
only once this version is past its update probation (it connected to
IronFlock and stayed up; `selfupdate`), so that a rollback to the previous
agent, which reads only the registry environment, still has its proxy. The
agent waits for that in the background; nothing waits on it. It is also not
done while the layout migration of a device from before 1.4.0 is under way,
nor while the agent runs as the old `reagent` service: the move picks it up
at a later start.

Every step that fails is logged once and the agent goes on with what it has:
a store it cannot read or write leaves the process environment as Windows
gave it (which still holds the registry values when they are there); an ACL it
cannot repair is logged as a warning; the registry cleanup is tried three
times and then left to the next start. None of it stops the agent or delays
its connection.

## Capability probe

```
device-agent.exe service capabilities
```

The command itself needs no administrator, but the installed
`device-agent.exe` lies in the agent's folder, which only SYSTEM and
Administrators can open: a standard user cannot run it at all (PowerShell:
`CommandNotFoundException`). So a caller runs the probe as administrator. A
caller without administrator rights tells a new agent by the registry value
`ProxyConfigured` (above): an agent with this contract writes it at its
install and at every start, an older one never.

Prints one capability per line on stdout, exit 0:

```
service-capabilities
proxy-store
service-configure
service-proxy
```

A caller decides by the line `service-configure`, never by the exit code
alone and never by a version number. What older agents answer:

- 1.6.19 and every agent with the `service` subcommand (0.18.20 to 1.6.19): exit 2,
  nothing on stdout, `unknown service command "capabilities"` and the usage
  line on stderr.
- an agent from before the `service` subcommand: exit 0 with `'-config'
  argument is required. -help for usage` on stdout and no capability line.

## `service configure`

```
device-agent.exe service configure [-http-proxy URL] [-https-proxy URL] [-no-proxy LIST]
                                   [-proxy-password-file PATH|-] [-remove-proxy] [-no-restart]
```

Run as administrator (elevated). Changes the proxy of the installed service
(`ironflock-device-agent`, or `reagent` before its move to 1.4.0).

Flags:

- `-http-proxy URL`, `-https-proxy URL`: the proxy for each protocol. `http`,
  `https`, `socks5` or `socks5h`; one without a scheme is `http://`; a host,
  nothing after it but `/`; no space, quote, backslash or control character.
  A URL never carries a password: `http://user@host:port` takes it from
  `-proxy-password-file`, `http://user:@host:port` is the empty password and
  takes no file. A URL with a non-empty password is a usage error (exit 2),
  and no message repeats the URL.
- `-no-proxy LIST`: the operator's own hosts reached directly, comma-separated,
  no spaces or quotes. Stored as given (entries trimmed, empty ones dropped);
  they replace the stored ones. `-no-proxy ""` removes them. What the device
  reaches directly is always added at the agent's start (see "The agent's
  NO_PROXY"); it is not stored.
- `-proxy-password-file PATH`: the password, the first line of the file
  (`\r\n` or `\n` ends it; at most 4096 bytes; an empty line or one with a
  control character is an error). `-` reads standard input instead: the
  simplest for a program, which writes the password and a newline to the
  command's stdin and closes it.
- `-remove-proxy`: remove the proxy: the store is deleted. Takes no other
  proxy flag.
- `-no-restart`: write the settings, do not restart the service; they apply
  at its next start.

Merge rules (the same as `ironflock agent configure` on Linux):

- a flag that is not given keeps its stored value;
- with `-proxy-password-file` and no URL flag, the password goes into each
  stored URL that names a user (also replacing one there); none names a
  user: exit 3;
- no URL flag and no stored proxy (and not `-remove-proxy`): exit 3;
- the stored `NO_PROXY` (the operator's entries) is the `-no-proxy` list when
  it is given, none for a device that had no proxy, otherwise it stays;
- at least one flag above (other than `-no-restart`) must be given: exit 2.

Then it writes the store atomically with its ACL (or deletes it), writes
`ProxyConfigured`, and, while the agent's own move is pending (the registry
environment still holds proxy values, see O2), writes the same values there,
with `NO_PROXY` as the whole computed list, which the older agent reads as it
is (or removes them with `-remove-proxy`).

Restart: when the settings changed and the service runs, it stops the service
(waiting up to 30 s), starts it, and waits up to 60 s for it to report running.
A stopped service is left stopped. Unchanged settings restart nothing.
Inside the agent's own process tree a restart is refused before anything is
written (O1).

Output, stdout, one `name=value` per line, in this order, nothing else:

```
proxy.changed=true
proxy.configured=true
proxy.http=http://user@proxy.example.com:3128
proxy.https=http://user@proxy.example.com:3128
proxy.no_proxy=historian.corp.example
proxy.no_proxy_effective=localhost,127.0.0.1,::1,historian.corp.example,.historian.corp.example,edge-pc-7,.edge-pc-7,10.20.30.0/24,appliance.example.com,.appliance.example.com
proxy.password=true
proxy.store=C:\ProgramData\IronFlock\DeviceAgent\proxy.env
service.name=ironflock-device-agent
service.restarted=true
service.state=running
```

- `proxy.http`, `proxy.https`: the URL without its password (empty when
  none). The empty password (`user:@`) is shown as it is.
- `proxy.no_proxy`: the operator's own entries, as stored.
- `proxy.no_proxy_effective`: the list the agent's process uses, computed
  now as the agent computes it at its start (empty without a proxy).
- `proxy.password`: a URL holds a non-empty password.
- `service.restarted`: true when it stopped and started the service.
- `service.state`: `running`, `stopped`, `start pending`, `stop pending`, or
  `unknown`.

Messages for a person go to stderr. Exit codes:

| Code | Meaning |
|---|---|
| 0 | done (also: nothing to change) |
| 1 | failed; the stored settings are as before |
| 2 | wrong command line; nothing changed |
| 3 | precondition: not elevated, service not installed, the layout migration is under way, restart refused inside the agent (O1), nothing to change a password or URL in; nothing changed |
| 5 | the settings are written, but the restart failed or the service did not report running in time; the output lines are printed, the settings apply at the next start |

## `service proxy`

```
device-agent.exe service proxy
```

Reads the stored proxy without the password. No flags. Stdout, `name=value`
lines in this order:

As administrator (elevated):

```
proxy.readable=true
proxy.configured=true
proxy.http=http://user@proxy.example.com:3128
proxy.https=http://user@proxy.example.com:3128
proxy.no_proxy=
proxy.no_proxy_effective=localhost,127.0.0.1,::1,edge-pc-7,.edge-pc-7,10.20.30.0/24
proxy.password=true
proxy.store=C:\ProgramData\IronFlock\DeviceAgent\proxy.env
proxy.source=store
```

`proxy.source`: `store`, or `service-environment` while the agent's move is
pending and the registry environment holds values that differ from the store
(those are in effect), or `none`.

As any other user (only from a copy of `device-agent.exe` outside the agent's
folder, see the probe; a caller can as well read `ProxyConfigured` itself):

```
proxy.readable=false
proxy.configured=true
```

`proxy.configured` is `true`, `false` or `unknown` (from `ProxyConfigured`).

Exit 0; 3 when the service is not installed (as any other user: when
neither service key exists); 1 when the store exists but cannot be read (as
administrator).

An administrator caller that needs the password (a connection test of
`doctor`) reads the store itself (format above).

## `service install`

Unchanged for people who use it by hand: `-proxy URL` (both protocols, may
still carry a password), `-no-proxy LIST`, `-force-proxy`. New, with the rules
of `service configure`: `-http-proxy`, `-https-proxy`, `-proxy-password-file`
(not together with `-proxy`). An existing store wins over the flags unless
`-force-proxy`, as before.

What changed: the store is written atomically with its ACL (an existing one
gets its ACL checked and repaired), `ProxyConfigured` is written, the
service's registry environment gets no proxy value, and the store's
`NO_PROXY` holds the `-no-proxy` entries alone (the agent computes the rest).

## The agent's NO_PROXY

The list the agent's process uses (Go's proxy rule reads it, which matches
address ranges), computed at every start of the service, and by `service
configure` and `service proxy` for their output, in this order, each entry
once, lower case, never with a port (Go narrows an entry with a port to that
port), names as `name,.name`, loopback as `localhost,127.0.0.1,::1`:

1. `localhost`, `127.0.0.1`, `::1`;
2. the operator's own entries (the store's `NO_PROXY`), always kept, a port
   taken off;
3. the machine's names: its host name and its DNS name with the domain;
4. the network of every interface that is up, as a range: IPv4, and IPv6
   except link-local (`fe80::/10`); loopback interfaces are covered by 1;
5. from the device's `.flock`: the hosts of `device_endpoint_url`,
   `docker_registry_url`, `update_url` and `insecure-registries`, then
   `appliance_domain`. All of them for a device of an appliance (it has an
   `appliance_domain`, or its endpoint is an address, which a cloud
   endpoint never is). A cloud device gets only those inside one of its
   networks, and the hosts of plain-HTTP registries (a registry with an
   explicit port, the rule `service install` always had): its endpoints are
   public, and keeping them off the proxy would take a device on a
   proxy-only network offline.

Nothing in it waits on the network; what cannot be read (the `.flock`, the
interfaces) is logged and left out, and the start goes on.

On Linux the agent computes nothing: the `ironflock` CLI writes the Linux
agent's list into `/etc/ironflock/proxy.env`.

App containers get, as before, the agent's list, then `localhost`,
`127.0.0.1`, `::1`, `host.docker.internal`, `10.0.0.0/8`, `172.16.0.0/12`,
`192.168.0.0/16`, the device's LAN address and the appliance's domain as
`name,.name`, and now also the names of the app's own compose services as
`name,.name` (as `NO_PROXY`, `no_proxy` and `IRONFLOCK_NO_PROXY`). No IPv6
range and no bracketed address (`[::1]`) goes into a container: Python's
httpx 0.28 (used by the Anthropic SDK, among others) raises an error on
either when it builds a client. Plain `::1` stays.

## Docker (for the caller; the agent does not touch Docker's proxy)

- Docker Desktop: the agent service has no `DOCKER_HOST`; the agent talks to
  `npipe:////./pipe/docker_engine`. Desktop's proxy is its own setting
  (Settings, Resources, Proxies) of the signed-in user.
- Docker Engine for Windows (no Desktop, `%ProgramData%\docker` exists): no
  `DOCKER_HOST` either; its daemon config is `%ProgramData%\docker\config\daemon.json`.
- WSL Docker Engine (`setup-docker-wsl.ps1`, then `connect-agent-to-wsl-docker.ps1`
  in this repository's root):
  - The engine is the WSL distribution `Ubuntu-24.04` (virtual disk in
    `C:\WSL\Ubuntu-24.04`), owned by the local account `wsldocker`. `wsl.exe`
    refuses to run as SYSTEM, so every command in the distribution runs as
    that account through the scheduled task `WSL-Runner`, by the helper
    `C:\WSL-Setup\run-as-wsl-user.ps1 -Bash '<command>'` (bash as root in the
    distribution) or `-PowerShell '<command>'` (PowerShell as `wsldocker`),
    from an administrator or SYSTEM shell. Its output is printed; logs in
    `C:\WSL-Setup\*.log`.
  - dockerd's proxy is the systemd drop-in
    `/etc/systemd/system/docker.service.d/http-proxy.conf` (mode 0600) in the
    distribution: `[Service]` and `Environment="HTTP_PROXY=…"`,
    `Environment="HTTPS_PROXY=…"`, `Environment="NO_PROXY=…"` (backslash and
    double quote escaped, `%` doubled). `setup-docker-wsl.ps1` writes it from
    the agent's proxy when it runs; a change needs `systemctl daemon-reload`
    and `systemctl restart docker` in the distribution.
  - How to know the agent uses it: the service's registry environment holds
    `DOCKER_HOST=tcp://127.0.0.1:2376`, `DOCKER_TLS_VERIFY=1`,
    `DOCKER_CERT_PATH=C:\WSL-Setup\certs` (and `PATH`), written by
    `connect-agent-to-wsl-docker.ps1`. dockerd listens there with mutual TLS
    (loopback only); the client certificates are in `C:\WSL-Setup\certs`.
    The agent never sends this loopback address through a proxy (Go never
    proxies loopback).
  - `setup-docker-wsl.ps1` takes the agent's proxy from its store (this
    file's format) when it runs as administrator, from the registry
    environment of an older agent otherwise, or from its own `$Proxy`
    setting.

## Changes to this contract

- 2026-11-17: first version.
- 2026-11-17: tried on Windows Server 2025: a standard user cannot run the
  installed `device-agent.exe` (the agent's folder is closed to them), so the
  capability probe and `service proxy` run as administrator, and a caller
  without administrator rights reads `ProxyConfigured` from the registry
  (whose presence also marks an agent with this contract). `service proxy`
  without administrator rights no longer opens the service manager, which
  refused a standard user.
- 2026-11-17: the owner's rule "the proxy settings always include a no_proxy
  setting for everything reachable locally without a proxy" (what is
  directly connected, not all private ranges): the store's `NO_PROXY` holds
  the operator's entries only; the agent computes its list at every start
  (new section "The agent's NO_PROXY"); `service configure` and `service
  proxy` print `proxy.no_proxy_effective` after `proxy.no_proxy`;
  `-no-proxy` stores its list as given instead of a list with the device's
  hosts; app containers also get the compose service names, and no IPv6
  range or bracketed address.



# IronFlock Agent


<p align="center">
  <img src="assets/coverage-badge.svg" alt="Coverage">
</p>

The _IronFlock Agent_ is a (lightweight) daemon running on IoT devices that provides
an interface to manage containers on the device and to collect/request app logs.
In particular, the daemon enables the
[IronFlock IoT Development Studio](https://www.ironflock.com/reswarm)
to authenticate and securely connect to an IoT device in order to control apps
running in containers on the device and retrieve their result and logs.

## Overview

- [IronFlock Agent](#ironflock-agent)
  - [Overview](#overview)
  - [Introduction](#introduction)
  - [Usage](#usage)
    - [Linux layout and service](#linux-layout-and-service)
  - [Development](#development)
  - [Build, Publish and Release](#build-publish-and-release)
    - [Release via CI (tag push)](#release-via-ci-tag-push)
    - [Promote a version](#promote-a-version)
    - [Update signatures (Ed25519)](#update-signatures-ed25519)
    - [One-time CI setup (Workload Identity Federation)](#one-time-ci-setup-workload-identity-federation)
    - [Targets](#targets)
    - [Target platform and architecture limitations](#target-platform-and-architecture-limitations)
    - [Versioning](#versioning)
    - [Local build/publish (fallback)](#local-buildpublish-fallback)
  - [Implementation](#implementation)
    - [WAMP](#wamp)
      - [References](#references)
    - [Docker](#docker)

## Introduction

## Usage

The _IronFlock Agent_ is provided as a statically linked binary and accepts various
CLI parameters to configure it when launched. To show a list of the parameters
it accepts apply the help parameter `./device-agent -help`

```Shell
Usage of ./device-agent:
  -agentDir string
    	default location of the agent binary (default "/opt/ironflock/device-agent" (linux), see "Linux layout")
  -appsDir string
       default path for apps and app-data (default (default agentDir) + "/apps")
  -arch
       displays the architecture for which the binary was built
  -compressedBuildExtension string
       sets the extension in which the compressed build files will be provided (default "tgz")
  -config string
       reswarm configuration file
  -connTimeout uint
       Sets the connection timeout for the socket connection in milliseconds (0 means no timeout) (default 1250)
  -dbFileName string
       defines the name used to persist the database file (default "device-agent.db")
  -debug
       sets the log level to debug (default true)
  -debugMessaging
    	enables debug logs for messenging layer
  -env string
    	determines in which environment the agent will operate. Possible values: (production, test, local) (default "production")
  -logFile string
       log file used by the agent (default "/var/log/ironflock/device-agent.log" (linux))
  -nmw
    	enables the agent to use the NetworkManager API on Linux machines (default true)
  -offline
       starts the agent without establishing a socket connection. meant for debugging (default=false)
  -ppTimeout uint
       Sets the ping pong timeout of the client in milliseconds (0 means no timeout)
  -prettyLogging
       enables the pretty console writing, intended for debugging
  -profiling
       sets up a pprof webserver on the defined port
  -profilingPort uint
       port of the profiling service (default 80)
  -remoteUpdateURL string
    	download base for agent updates (the .flock update_url wins) (default "https://instance-registry.ironflock.com/dl")
  -respTimeout uint
       Sets the response timeout of the client in milliseconds (default 5000)
  -update
       determines if the agent should update on start (default true)
  -version
       displays the current version of the agent
```

The `config` parameter needs to be populated with the path to a local `.flock` file. This `.flock` file contains all the neccessary device configuration and authentication data required to run the agent.

Read more on `.flock` files and how they work here: https://ironflock.com/en/docs/device-management/flockflasher

**Example Usage**
```
./device-agent -config path/to/config.flock -prettyLogging
```

### Linux layout and service

Without `-agentDir`/`-dbFileName`/`-logFile` the agent picks its paths from
what it finds at every start ([`src/layout`](src/layout/layout.go)), so it
never opens an empty database next to a populated one:

| Layout (`stats.layout`) | Found | Agent folder, database, log |
|---|---|---|
| `ironflock` | `/opt/ironflock/device-agent`, or nothing | `/opt/ironflock/device-agent`, `device-agent.db`, `/var/log/ironflock/device-agent.log` |
| `legacy` | `/opt/reagent` from ironflock-init < 1.4.0 (`reagent.service` linked into `/opt/reagent/reswarmify/services/`) | `/opt/reagent`, `reagent.db`, `/var/log/reagent.log` |
| `legacy-image` | `/opt/reagent` from an OS image (FlockOS, ReswarmOS, Jetson, …) | as `legacy`, never moved |

In the current layout one systemd unit runs the agent, and the agent activates
its own updates exactly as the Windows service does (below):
`device-agent service install -config <flock> [-start]` (root) writes
`/etc/systemd/system/ironflock-device-agent.service`, `repair.sh` and the
0600 config copy; `service status|start|stop|restart|uninstall` wrap
systemctl.

**Updates never cost a device its connection to IronFlock** — the only way to
fix anything on it ([`src/selfupdate`](src/selfupdate)):

- The agent updates only when it starts and when a user asks for it (Studio's
  update button) — never on its own while it runs, which would interrupt the
  device's service. The check at start runs before the agent connects, so a
  fix release reaches a version that cannot connect whenever it starts again
  (after a rollback, a crash, a reboot).
- It activates an update only while it is connected (the version it replaces,
  the rollback target, is then known to connect) — except after 24 hours away,
  when the running version may be what keeps the device off.
- Updates download as `device-agent-v<ver>`, are validated, swapped in with
  `link(device-agent → device-agent-prev)` + `rename`, and the agent exits
  with code 75 for systemd to restart it.
- Probation: the new version must start, become ready and **connect to
  IronFlock within 10 minutes**, and stay up 2 minutes. A version that fails 3
  starts in a row, or does not connect in time, is rolled back.
- The old version then judges it, by the same bar (ready, connected, up 2
  minutes): if it passes, the new one failed where the old one works — one
  strike; it is tried once more at the next update (a start, or a user's),
  and a second strike
  blocks it until a newer release appears (one failure can be an outage that
  ended just in time for the old version). If the old one does not pass
  either within 10 minutes, the network was at fault, and the new one may be
  tried again. If the old one keeps dying before it can, the device switches
  back to the new one — it alternates until one connects. Two broken releases
  in a row both end blocked; the device stays on the version that connects.

**Moving legacy installs (agent 1.3.0).** Every legacy install moves: ironflock-init's,
FlockOS, ReswarmOS, the Cubic image, Jetson, the installers of 2021–23,
appliance hosts. The move restarts Docker and every app once, so like an
update it starts only at the agent's start — a legacy agent tries once, 5
minutes after it connected to IronFlock (1.3.0 itself arrives at a start or
by a user's update) — and when a user asks for it: Studio's update button on
a legacy device with nothing newer to install, or `device-agent migrate-layout
start` in the device terminal. It runs `device-agent migrate-layout` in a unit
of its own, `ironflock-device-agent-migrate.service`. How it moves is
planned per device ([`src/migrate/plan.go`](src/migrate/plan.go)):

- Where `/opt/reagent` can be renamed as a whole, it moves (database, Docker's
  data-root, frpc, everything) to `/opt/ironflock/device-agent` with one
  rename, leaves a link `/opt/reagent → /opt/ironflock/device-agent`, and
  `daemon.json` follows. Docker and the apps restart once; containers are not
  recreated.
- Where it cannot (a mount at or below it, another filesystem than `/opt`, an
  OS image), it stays, and `/opt/ironflock/device-agent` becomes a link to it.
- The legacy units (links into `/opt/reagent`, real files, or package files)
  are stopped, disabled and masked; a real unit file is moved aside first.
  The new unit takes the legacy unit's `-config`, `-appsDir` and other flags.
- The config's link into `/boot` becomes a 0600 file and the `/boot` copies
  of the device secret are deleted — except on OS images (FlockOS, ReswarmOS,
  Cubic), whose boot scripts relink the config from `/boot` and re-enable the
  legacy units: there the `/boot` files stay and the legacy units stay
  masked. An OS update brings the image's legacy layout back with its agent,
  which updates and moves again.

The move is done only once the new agent **connects to IronFlock** and stays up
a minute. Each step is journaled in `/opt/ironflock/.device-agent-migration.json`
and the unit restarts a migration process that dies, so a power cut or a
crash resumes; a failure undoes every step and restarts the legacy agent,
which connects again. The heartbeat reports `stats.layout` and, after a
rollback, `stats.layout_error`; the device tries again at a later start (a
day after the failure at the earliest), without end, or when a user asks.

- Jetson installs have no supervisor that activates agent updates: they never
  run 1.3.0 by themselves (reinstall them with ironflock-init).
- `device-agent migrate-layout status` prints the journal.
- `gs://ironflock-downloads/device-agent/migration.json` with
  `{"enabled": false}` stops new migrations fleet-wide (absent: enabled).
  It can only hold devices back, so it is not signed. Every promotion publishes
  it as enabled when it is missing: the bucket is public by object name only,
  so a missing object answers 403, which the agent does not take as "absent".
- Test hooks (the e2e in device-init): `IRONFLOCK_MIGRATE_FAULT=<step>` fails
  at a step, `IRONFLOCK_MIGRATE_PAUSE=<step>` stops after it; set them in a
  `reagent.service.d` drop-in. Steps: `prepare`, `block-docker`,
  `stop-legacy`, `stop-docker`, `rename-database`, `move-folder`,
  `docker-data-root`, `config-file`, `logs`, `unblock-docker`,
  `start-agent`, `health`, `cleanup`.

### Running as a Windows service

On Windows the agent should be installed as a service instead of being started
manually — the service starts at boot (no logged-in user required), restarts
on failure via SCM recovery actions, and **activates self-updates** (a console
agent only downloads them). From an elevated (Administrator) prompt:

```
device-agent.exe service install -config path\to\config.flock
reagent.exe service status|start|stop|uninstall
```

(The download is `device-agent.exe` since 1.3.0; the service still installs it as
`reagent.exe` in `%ProgramData%\IronFlock\Reagent` until the Windows layout moves.)

`service install` **starts the service** when it finishes — a device that was just
registered should come online without a second command. Pass `-noStart` to install
without starting. (`-start` is still accepted for compatibility with older docs and
provisioning scripts, but it no longer does anything.)

What `service install` sets up:

- Copies the exe and the `.flock` (as `device.flock`) into the agent dir
  (default `%ProgramData%\IronFlock\Reagent`, override with `-agentDir`) and
  bakes `-config/-agentDir/-appsDir/-dbFileName/-logFile` into the service
  ImagePath, so no path ever falls back to `os.UserHomeDir` (meaningless under
  LocalSystem).
- Restricts the agent dir ACLs to SYSTEM + Administrators (the `.flock` and
  generated `.env-compose` files contain the device secret; the self-update
  executes binaries from this dir as SYSTEM), with a Users modify-grant on the
  apps dir for Docker Desktop bind mounts.
- Recovery actions: restart after 5s/30s/120s, repeating forever (reset period
  1 day). Deliberate restarts (update activation, `system_restart_agent`) exit
  without reporting SERVICE_STOPPED so they always trigger recovery.
- A boot-time repair Scheduled Task ("IronFlock Agent Repair") that restores
  `reagent.exe` from `reagent-prev.exe` if an interrupted update swap left the
  ImagePath vacant — SCM recovery cannot fix a service that fails to _start_.
- Optional `-proxy http://host:port` writes HTTP(S)_PROXY into the service
  environment (a LocalSystem service only sees machine-wide env).

Update flow (the same on Linux, see [Linux layout and service](#linux-layout-and-service)
and `src/selfupdate`): download `reagent-v<ver>.exe` (SHA-256 and signature
verified) → validate it executes → rename the running exe to
`reagent-prev.exe`, rename the update into place → exit for a supervised
restart → probation: a new version that fails 3 consecutive starts, or does
not connect to IronFlock within 10 minutes, is rolled back; it is blocked once
the old version connects (and may be tried again when the old one cannot
either: the network was at fault).

Docker note: with Docker Desktop the engine only starts at user sign-in. The
service tolerates that (it waits patiently and reports the device as
CONFIGURING), but for unattended devices enable Docker Desktop autostart plus
Windows auto sign-in.

The device terminal works on Windows 10 1809 (build 17763) and newer: the agent
opens a Windows pseudoconsole (ConPTY) running PowerShell 7 where it is
installed and Windows PowerShell otherwise, in place of the bash pty it uses on
Linux. Because the agent runs as LocalSystem, that terminal is a SYSTEM shell —
the same posture as the root shell the Linux terminal gives. On older builds the
terminal reports that the pseudoconsole API is missing rather than failing
obscurely.


## Development

The agent embeds the frp client into the binary. For that to work locally you first build frpc for your machine from the `frp/` module (the release builds it the same way for every target).

```Shell
just build-frpc
```

Once Go has been [downloaded and installed](https://go.dev/doc/install), users can run the project locally by running the following command :

```Shell
just run

# or on a mac
just run_mac
```

During development the `/opt/ironflock/device-agent` folder is used as the agent directory (`/opt/reagent` if that exists, see [Linux layout and service](#linux-layout-and-service)). There you can find additional config like the frpc.ini for the tunnel configuration.
To test with a local test device use the `test-config.flock` file when connecting to the local dev environment. A few things might need to be adjusted according to your environment.
The secret must be the one from the device's database record. Also use the insecure (ws instead of wss) endpoint and make sure the swarm_key and device_key are set.

If you encounter any privilege issues, please try removing the agent home directory beforehand (by default found in `${HOME}/reagent`) or try running `go` as root.

## Build, Publish and Release

Building, publishing and promoting is done by **CI** (GitHub Actions), from a tag on a commit merged into `main`. Only code whose pull-request checks passed reaches a device; nobody signs or promotes from a laptop, except the one-time bridge release and emergencies (see [Promote a version](#promote-a-version)).

### Release via CI (tag push)

1. `just release-patch` (or `release-minor` / `release-major`) — the whole local side in one command, run from an up-to-date `main`: refuses a dirty tree, bumps [`src/release/version.txt`](src/release/version.txt) + `availableVersions.json`, and opens a pull request with just those two files (`release reagent vX.Y.Z`). `main` is protected — every change, the version bump included, merges only through a pull request whose required checks (`ci-ok`, `security / report`) passed — so GitHub merges the release PR automatically once they are green. The recipe then tags the merged commit `vX.Y.Z`, pushes the tag and waits for the release run.

   Commit your own work first, with your own message — the release commit carries the version bump alone. This is the shared IronFlock release contract; see [`REDeployments/docs/RELEASE.md`](../deployments/docs/RELEASE.md).

   For a hand-managed version, bump `version.txt` yourself (see [Versioning](#versioning)) in a pull request; once it is merged, tag the merged commit `vX.Y.Z` and push the tag. The release workflow only accepts a tag on a commit that is part of `main` and matches `version.txt`.
2. [`.github/workflows/release.yml`](.github/workflows/release.yml) then, for every entry in the [`targets`](targets) file: builds the binary, generates a CycloneDX SBOM, records a sigstore-signed **SBOM attestation** bound to the binary's digest, publishes the binary to `gs://ironflock-downloads/device-agent/<os>/<arch>/<version>/device-agent[.exe]`, and pushes the multi-arch agent images to Artifact Registry. This **stages** the version: the binaries exist, unsigned, and no agent updates to it yet.
3. Its `promote` job then signs and **promotes** the version (`just promote-ci`, see the next section): from this moment agents update to it. The channels are those in the tagged `availableVersions.json`, which `just release-patch` sets to the new version.

A manual run from the Actions tab (`workflow_dispatch`) is a dry run: it builds, but neither publishes nor promotes.

Verify a published binary's provenance any time with:
```Shell
gh attestation verify device-agent-linux-amd64 --repo IronFlock/device-agent
# up to v1.2.13 the build was named reagent-<os>-<arch>; up to v1.2.8 attested before the repo moved from RecordEvolution:
gh attestation verify reagent-linux-amd64 --owner IronFlock --signer-repo RecordEvolution/DeviceManagementAgent
```

### Promote a version

Promotion signs every binary that `availableVersions.json` names, and the pinned frpc ([Update signatures](#update-signatures-ed25519)), republishes the [`latest` alias](#the-latest-alias), then publishes `version.txt`, `availableVersions.json` and the signed channel map. The release workflow's `promote` job does this for every tag:

```Shell
just promote-ci   # CI only: signs with Cloud KMS, publishes manifest-v2.json
```

The job runs only on a pushed tag, and it refuses a version below the live `version.txt`: re-running an old release cannot roll the fleet back.

Agents read the signed channel map `gs://ironflock-downloads/device-agent/manifest-v2.json`: an envelope `{"payload": <base64>, "signature": <.sig>}` whose payload is `{"channels": <availableVersions.json>, "issued_at": …}`, signed like a binary but with the context `device-agent-manifest:v1` and the line `device-agent/manifest-v2.json`. On a 404 (an appliance mirror or a bucket without it) they read `manifest.json`, and on a 404 there the frozen legacy tree. A map that does not verify is refused, never routed around. The plain `availableVersions.json` next to it is for the flasher and older tooling. This publish **is** the release gate.

**The bridge release.** Agents up to 1.3.4 trust only the OpenBao key and read only `manifest.json`. The version in [`src/release/bridge-version.txt`](src/release/bridge-version.txt) is the first that trusts the Cloud KMS key too and reads `manifest-v2.json`. CI stages it but does not promote it; its promotion is the last one signed through OpenBao, by hand:

```Shell
just promote-bridge   # once: signs through OpenBao, publishes manifest.json AND manifest-v2.json
```

That freezes `manifest.json` at the bridge release: older agents and ironflock-init land on it, and the bridge release follows `manifest-v2.json` from then on. CI refuses to promote a later version until the bridge release is live.

**Emergencies** (a rollback, Cloud KMS unavailable): set `availableVersions.json`, then `just promote`, which signs through OpenBao and publishes `manifest-v2.json`; every agent from the bridge release on trusts both keys. (To promote only the version lists: `just publish-latestVersions`, which checks that each sealed `build/<manifest>` names what `availableVersions.json` says.)

#### The frozen legacy tree `gs://re-agent`

Agents before 1.3.0 only know `gs://re-agent/<os>/<arch>/<version>/reagent[.exe]` and its unsigned `availableVersions.json`. With 1.3.0, `just freeze-legacy 1.3.0` copies that version there under the legacy names, signs it as `reagent`, aliases it to `latest/` and points every legacy channel at it — once. However late an old agent comes online, it updates to 1.3.0, which reads the new tree from then on and moves its device to the new layout. Nothing is published to `gs://re-agent` after that.

#### The `latest` alias

Every promotion first runs `just publish-latest-alias`, which server-side-copies every binary staged under the **`production`** version to a `latest` version segment:

```text
gs://ironflock-downloads/device-agent/windows/amd64/<version>/device-agent.exe  ->  …/windows/amd64/latest/device-agent.exe
gs://ironflock-downloads/device-agent/linux/arm64/<version>/device-agent        ->  …/linux/arm64/latest/device-agent
...  (every os/arch in `targets`, plus the .sha256 and .sig sidecars for each)
```

This exists for the **manual install surface only**. Linux devices resolve the version themselves (ironflock-init reads the manifest), but Windows has no such installer — the user downloads `device-agent.exe` from a link in the docs and the platform UI, so that link must not carry a version. Those links point at:

```text
https://instance-registry.ironflock.com/dl/device-agent/windows/amd64/latest/device-agent.exe
https://instance-registry.ironflock.com/dl/device-agent/linux/<arch>/latest/device-agent
```

(`/dl/device-agent/*` is the registry-proxy route; it forwards verbatim to `https://storage.googleapis.com/ironflock-downloads/device-agent/*`, so the raw GCS URL works identically. `/dl/re-agent/*` keeps serving the frozen legacy tree.)

The copy is byte-for-byte, so the Authenticode signature applied by `scripts/sign-windows.sh`, the `.sha256` sidecar and the `.sig` update signature published by `scripts/publish.sh` all stay valid. The alias objects are written with `Cache-Control: public, max-age=0` so they are always revalidated.

Because it runs before `publish-latestVersions`, it also acts as a preflight guard: promoting a version whose binaries the release CI never published, or published without a `.sig`, fails here, leaving the live manifest untouched.

> **Never put `"latest"` into `availableVersions.json`.** The agent's OTA path feeds that value to `compareVersion()` ([`src/system/system.go`](src/system/system.go)) as a semver constraint, which would fail to parse. The alias is for humans; agents always resolve a concrete version.

### Update signatures (Ed25519)

The agent installs an OTA update, and on Windows the frpc it downloads, only if `<binary>.sig`, published next to the binary and its `.sha256`, verifies against a public key compiled into the running agent (`AGENT_UPDATE_PUBLIC_KEYS` in [`src/updatesig/updatesig.go`](src/updatesig/updatesig.go)). The `.sig` holds the standard base64 of the raw 64-byte Ed25519 signature over

```text
reagent-update:v1            (frpc: frpc-download:v1)
<os>/<arch>/<version>/<file> e.g. linux/armv7/1.3.0/device-agent, windows/amd64/0.71.0/frpc.exe
<sha256 of the binary, lowercase hex>
```

joined by `\n`, no trailing newline. The agent builds the middle line from what it is downloading (the version it resolved, its own platform, the file name), so a server cannot pass an old signed release off as a new one, or one platform's binary as another's. Every server that hands out agent or frpc downloads, including an appliance's agent mirror, must serve the `.sig` at the same path as the `.sha256`.

**Where the keys live.** Neither private key exists in GitHub or on a laptop, and neither can be exported:

- **Cloud KMS** `projects/record-1283/locations/europe-west1/keyRings/ironflock-release/cryptoKeys/agent-update` (Ed25519, key ID `4193db1f`) signs every release after the bridge release. Only the service account `agent-signer` may sign with it (`roles/cloudkms.signer` on this key alone), and only the `promote` job of a tag run of this repository's `release.yml` may act as that account (see [One-time CI setup](#one-time-ci-setup-workload-identity-federation)).
- **OpenBao** transit key `agent-update` (ed25519, `exportable=false`, key ID `2b040fcf`) signed every release up to the bridge release, and `manifest.json`. It stays trusted, so that everything it signed keeps verifying, and it is the emergency signer. The maintainer reaches OpenBao through kubectl only; [`scripts/openbao-admin.sh`](scripts/openbao-admin.sh) runs `bao` in `openbao-0` as role `admin` (context `OPENBAO_KUBE_CONTEXT`, default `gke_record-1283_europe-west1-b_ironflock-prod`).

Each key backs up the other: the fleet trusts both, so losing one means signing with the other while a release replaces it.

**One-time setup of the OpenBao key** (fresh installs of the ironflock chart create the mount and key themselves):

```Shell
scripts/openbao-admin.sh 'bao secrets enable transit'
scripts/openbao-admin.sh 'bao write -f transit/keys/agent-update type=ed25519 exportable=false allow_plaintext_backup=false'
just agent-update-pubkeys    # paste the printed line(s) into AGENT_UPDATE_PUBLIC_KEYS
```

**Signing** happens in every promotion (also in `just publish-version` / `just publish-latestVersions`, so the channel map cannot flip unsigned), via `just sign-release` ([`scripts/sign-release.sh`](scripts/sign-release.sh); `SIGNER=kms` or `openbao`, `MANIFESTS` names the channel maps to seal): for every version `availableVersions.json` names and for the pinned frpc it downloads each binary from GCS (the channel maps it builds locally), hashes it locally, builds the message (`sign-update message`), and signs it: Cloud KMS with one `gcloud kms asymmetric-sign` call per message (`sign-update sig-from-kms`), OpenBao with all messages in one `kubectl exec` session (`sign-update sig-from-openbao`). It verifies every `.sig` against the keys compiled into this tree, and only then uploads them. A published `.sig` that already verifies is kept (when signing through OpenBao: only one the OpenBao key made, so that the bridge release verifies on agents that trust nothing else). A staged version that is already signed is never restaged by `publish.sh`; a signed frpc is never overwritten by `publish-frpc.sh` (bump `FRP_VERSION` instead).

**Rotation:** create a new version of the key (Cloud KMS: `gcloud kms keys versions create`; OpenBao: `scripts/openbao-admin.sh 'bao write -f transit/keys/agent-update/rotate'`), add its public key line to `AGENT_UPDATE_PUBLIC_KEYS` (KMS: `gcloud kms keys versions get-public-key`, the raw 32 bytes after the DER prefix, base64; OpenBao: `just agent-update-pubkeys`) and keep signing with the old version (`KMS_KEY_VERSION` / `OPENBAO_TRANSIT_KEY_VERSION`) until a release that trusts both reaches the fleet; then sign with the new version and drop the old line in a later release.

**Backup:** Cloud KMS holds its key; the OpenBao key is in OpenBao's Raft storage, which the OpenBao snapshots plus the seal key cover. Since the fleet trusts both keys, either can stand in for the other.

`AGENT_UPDATE_PUBLIC_KEYS` holds the OpenBao key's version 1 (key ID `2b040fcf`) and the Cloud KMS key's version 1 (key ID `4193db1f`). Should it ever hold the `TODO-AGENT-UPDATE-PUBLIC-KEY` placeholder, `TestProductionUpdateKeysConfigured` fails and blocks the release gate (locally, `AGENT_SIGNING_KEY_PLACEHOLDER_OK=1` skips it). To check a published binary by hand: `cd src && go run ./cmd/sign-update verify -version <version> [-kind device-agent|reagent|frpc -os <os> -arch <arch>] <file>` (with `<file>.sig` next to it); a channel map: `go run ./cmd/sign-update open -context device-agent-manifest:v1 -subject device-agent/manifest-v2.json manifest-v2.json` (the same with `manifest.json`). `sign-update keygen` / `sign` work with a local key file for development and tests only.

Emergency only (broken signing key): `AGENT_UPDATE_ALLOW_UNSIGNED=true` in the agent's service environment installs agent updates and frpc downloads whose signature is missing or invalid, with a warning each time. Releases published before signing existed have no `.sig`, so a verifying agent never installs them again.

### One-time CI setup (Workload Identity Federation)

The release workflow authenticates to Google Cloud **keylessly** via Workload Identity Federation — no long-lived key in GitHub. Set it up once:

```Shell
PROJECT=record-1283
POOL=github-pool
PROVIDER=github-provider
REPO=IronFlock/device-agent
REPO_ID=$(gh api repos/${REPO} --jq .id)   # 314514906 — immutable, survives renames and transfers
SA=reagent-release@${PROJECT}.iam.gserviceaccount.com

# 1. Workload Identity Pool + GitHub OIDC provider (locked to this repo by its numeric id)
gcloud iam workload-identity-pools create $POOL --project=$PROJECT --location=global --display-name="GitHub Actions"
gcloud iam workload-identity-pools providers create-oidc $PROVIDER \
  --project=$PROJECT --location=global --workload-identity-pool=$POOL \
  --display-name="GitHub" --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_id=assertion.repository_id" \
  --attribute-condition="assertion.repository_id=='${REPO_ID}'"

# 2. Service account with publish permissions
gcloud iam service-accounts create reagent-release --project=$PROJECT --display-name="reagent release"
gsutil iam ch "serviceAccount:${SA}:roles/storage.objectAdmin" gs://ironflock-downloads
gcloud projects add-iam-policy-binding $PROJECT --member="serviceAccount:${SA}" --role="roles/artifactregistry.writer"

# 3. Let the repo impersonate the SA via the pool
PROJNUM=$(gcloud projects describe $PROJECT --format='value(projectNumber)')
gcloud iam service-accounts add-iam-policy-binding $SA --project=$PROJECT \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJNUM}/locations/global/workloadIdentityPools/${POOL}/attribute.repository_id/${REPO_ID}"
```

**The signing identity** (Cloud KMS). The pool's provider maps `attribute.release` to `<repository_id>:release` only for a `v*` tag run of the repository's own `.github/workflows/release.yml` (or the shared release-image workflow); only that principal may act as `agent-signer`, and `agent-signer` may only sign with this key:

```Shell
gcloud kms keyrings create ironflock-release --location=europe-west1 --project=$PROJECT
gcloud kms keys create agent-update --keyring=ironflock-release --location=europe-west1 --project=$PROJECT \
  --purpose=asymmetric-signing --default-algorithm=ec-sign-ed25519 --protection-level=software
gcloud iam service-accounts create agent-signer --project=$PROJECT --display-name="Agent update signer (CI, keyless)"
gcloud kms keys add-iam-policy-binding agent-update --keyring=ironflock-release --location=europe-west1 --project=$PROJECT \
  --member="serviceAccount:agent-signer@${PROJECT}.iam.gserviceaccount.com" --role=roles/cloudkms.signer
gcloud iam service-accounts add-iam-policy-binding agent-signer@${PROJECT}.iam.gserviceaccount.com --project=$PROJECT \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJNUM}/locations/global/workloadIdentityPools/${POOL}/attribute.release/${REPO_ID}:release"
```

(Cloud HSM has no Ed25519; the key is software-protected, still non-exportable.)

Then add two **organization variables** in `IronFlock` (Settings → Secrets and variables → Actions → Variables; the provider condition limits them to this repo anyway):

- `GCP_WORKLOAD_IDENTITY_PROVIDER` = `projects/<projNum>/locations/global/workloadIdentityPools/github-pool/providers/github-provider`
- `GCP_SERVICE_ACCOUNT` = `reagent-release@record-1283.iam.gserviceaccount.com`

### Targets

The `targets` file, which can be found in the root of this project, determines which platforms and architectures the binary should be (cross-)compiled into.

The following platforms are set by default:
```
linux/amd64
linux/arm64
linux/arm/7
linux/arm/6
linux/arm/5
windows/amd64
darwin/amd64
```

Run `go tool dist list` to see all possible combinations supported by `go` in case you wish to add your own.

**NOTE: The IronFlock Agent only supports a limited amount of targets, please read more [here](#target-platform-and-architecture-limitations)**

### Target platform and architecture limitations

Due to this project using a [CGo-free port of SQLite/SQLite3](https://gitlab.com/cznic/sqlite), the IronFlock Agent can only be built into a [limited amount of target platforms and architectures](https://pkg.go.dev/modernc.org/sqlite?utm_source=godoc#hdr-Supported_platforms_and_architectures):

```
linux/386
linux/amd64
linux/arm
linux/arm64
linux/riscv64
windows/amd64
windows/arm64
darwin/amd64
darwin/arm64
freebsd/amd64
```

### Versioning

The version that is baked into the binary on build is determined by the string provided in the `src/release/version.txt` file. Adjust this file accordingly **before** making a build.

Once built the version of a binary can be verified with:
```
./device-agent -version
```

### Local build/publish (fallback)

CI (above) is the primary path. The original local recipes still work for ad-hoc builds and manual recovery, but require local `gcloud`/`docker` auth:

- `just build-all-docker` — cross-compile every target inside the builder image into `build/`.
- `just publish` — stage the `build/` binaries with their `.sha256` in the [ironflock-downloads](https://console.cloud.google.com/storage/browser/ironflock-downloads) bucket (unsigned; promotion signs them).
- `just push-docker-image` — build + push the multi-arch agent images.
- `just rollout` — **!!! USE WITH CAUTION !!!** build + publish + the emergency `promote` (which signs through OpenBao) (incl. `availableVersions.json`) in one local step; make sure `src/release/version.txt` and `availableVersions.json` are correct first.


## Implementation

The _Reagent_ makes use of [WAMP](https://wamp-proto.org)
and [Docker](https://www.docker.com) as its two key technologies.

### WAMP

#### References

- https://wamp-proto.org/_static/gen/wamp_latest.html
- https://godoc.org/github.com/gammazero/nexus/client#ConnectNet
- https://crossbar.io/docs/Challenge-Response-Authentication/

- https://github.com/gammazero/nexus
- https://github.com/gammazero/nexus/wiki
- https://sourcegraph.com/github.com/gammazero/nexus@96d8c237ee8727b31fbebe0074d5cfec3f7b8a81/-/blob/aat/auth_test.go

### Docker

In order to implement a _daemon_ running on an IoT device that is able to manage,
create, stop and remove _Docker containers_ we use the officially supported _Go_
SDK.

Please remember that on a default docker setup on a Linux platform we always
have to launch the daemon with root permissions, since the docker daemon is
accessed via the socket `///var/run/docker.sock` which is only readable with
root permissions by default.


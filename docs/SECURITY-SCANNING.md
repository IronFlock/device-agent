# Security / CVE screening

How we screen the agent for known vulnerabilities (CVEs) in our code and all
dependencies.

## What is screened (and what isn't)

The production artifact is the **bare, statically-linked Go binary** (`CGO_ENABLED=0`)
that runs directly on each edge device's host OS. So the screen targets exactly
what ships:

- ✅ **Our Go code + all module dependencies** — `go.mod` closure, including the
  nexus fork (`github.com/ironflock/nexus/v3`).
- ✅ **The embedded `frpc` binary** — built from source by the `frp/` module
  (`scripts/build-frpc.sh`, for every release target) and embedded into the agent
  (`src/embedded/frpc_binary`). `frp/go.mod` pins the frp version (= `FRP_VERSION`,
  checked by `scripts/check-frp-version.sh`) and raises `golang.org/x/net` and
  `x/crypto` above frp's own requirements. It is **not** in the agent's `go.mod`, so it
  is scanned separately in binary mode. Stripped like the upstream binary (size matters
  on devices), so binary mode reports whole modules; the one resulting false positive
  (`x/crypto/openpgp`, not linked) is recorded in `security/vex.openvex.json`.

Out of scope (deliberately):

- ❌ **The Docker images** — the builder (`Dockerfile`) is a throwaway build vehicle;
  the `ubuntu:24.04` runtime image (`docker/Dockerfile`) is only used for local dev
  against the REDeployments stack (its `agent` service). Neither ships to devices —
  devices run the static binary pulled from `gs://ironflock-downloads/device-agent`. Note the runtime image
  *is* pushed to `europe-docker.pkg.dev/.../ironflock-agent` on every tagged release
  (`release.yml` → `just push-docker-image`), so developers pull it; it just never
  reaches a device or a production cluster. Kept on **ubuntu** (not alpine) on
  purpose: its job is to resemble a real device, and devices are glibc Debian-family,
  so a musl base would make the one environment built for fidelity diverge from
  production. Bumped `22.04` → `24.04` on **2026-09-11** and given an
  `apt-get upgrade`, which took it from 133 CVEs (9 fixable) to **115 (0 fixable in
  the OS layer)**.
- ❌ **The edge host OS** — it's the customer's device, not something we ship.

## Tools

- **[`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)** (official
  Go vuln scanner) — does **reachability analysis**, so it only flags vulnerabilities
  your code can actually reach (low false-positive rate). It builds the code, so it
  honours the `replace` directive.
  - *source mode* (`./...`) — our code + all module deps.
  - *binary mode* (`-mode=binary`) — scans a compiled Go binary via its embedded
    build info; used for the **frpc** binary.
- **[`cyclonedx-gomod`](https://github.com/CycloneDX/cyclonedx-gomod)** — Go-native
  CycloneDX SBOM generator (module graph + binary).

Both run via `go run` and are version-pinned in the `Justfile` (`GOVULNCHECK_VERSION`,
`CYCLONEDX_GOMOD_VERSION`). No Docker, no other host tooling required.

## Running it

```bash
just security      # everything below, locally and report-only (CI blocks)

just vuln-go       # our Go code + module deps (exit 3 = reachable vulns found)
just vuln-frpc     # the embedded frpc binary (frp itself + its bundled deps)
just sbom          # CycloneDX SBOMs -> build/sbom/{reagent,frpc}-*.cdx.json
just sarif         # govulncheck SARIF -> build/sarif/ (uploaded to Security tab by CI)
just vuln-binaries # optional: scan each cross-compiled build/reagent-* artifact
```

`govulncheck` exits **3** when it finds reachable vulnerabilities — that's expected
locally. `just security` wraps the scans and always exits 0; the blocking decision
is CI's `security / report` job.

## CI

Both workflows call the shared IronFlock CI harness
([IronFlock/workflows](https://github.com/IronFlock/workflows), `@v1`).

| Check | What it runs | When |
|---|---|---|
| `ci / gate` | `just lint` (gofmt, `go mod tidy -diff`, go vet, build, golangci-lint incl. gosec for Linux **and** Windows, frp version pins), `just test` (race + shuffle, no cache), cross-builds of the agent with frpc for linux/amd64, arm64, arm/7, arm/6 and windows/amd64 | every PR, push to `main` |
| `ci / workflows` | actionlint, zizmor, pinned actions | every PR, push to `main` |
| `ci / integration`, `ci / coverage`, `ci / windows-unit` | Docker/D-Bus/frps integration suite, production-scoped coverage, native Windows tests | every PR, push to `main` |
| `security / security` | gitleaks, `just sarif` (govulncheck on the code and the embedded frpc), semgrep (IronFlock rules), trivy config (Dockerfiles), licences, `just sbom` | every PR, push to `main`, Mondays 06:17 UTC |

`ci-ok` (all ci jobs green) and `security / report` are the required checks of the
ruleset on `main`: nothing merges unless both pass. The weekly run matters because
CVEs are disclosed against code long after it is written.

**Blocking.** The `report` job fails on secrets, fixable high/critical
vulnerabilities, high misconfigurations, forbidden licences and semgrep errors.
Accepted findings carry a reason and an expiry and are reviewed in the PR that adds
them: `security/vex.openvex.json` (not-affected vulnerabilities, valid 12 months),
`.trivyignore.yaml` (`expired_at` required; the root user of the agent images),
`.gitleaks.toml` (test vectors and the local-dev demo device, each scoped to its
lines), and `//nolint:gosec // <reason>` at the code site.

## Signed SBOM attestation (release provenance)

The **release** workflow (`.github/workflows/release.yml`, tag `vX.Y.Z` / manual) attests
**the exact binaries it publishes**. For every target it builds the binary, generates a
per-binary CycloneDX SBOM (`just sbom-bin`), records a **sigstore-signed SBOM attestation**
bound to the binary's digest (`actions/attest-sbom`), and then publishes that same binary to
`gs://ironflock-downloads/device-agent`. No GHAS required. Verify any published binary with:

```bash
gh attestation verify device-agent-linux-amd64 --repo IronFlock/device-agent   # reagent-linux-amd64 up to v1.2.13
# releases up to v1.2.8 were attested before the repo moved from RecordEvolution:
gh attestation verify reagent-linux-amd64 --owner IronFlock --signer-repo RecordEvolution/DeviceManagementAgent
```

Because CI both attests and publishes the same bytes, the attestation matches what ships —
as long as releases go through CI (tag push) rather than a local `just rollout`.

## Remediation status

- **2026-10-22:** Go **1.27.3** (GO-2026-6809), `x/net` v0.61.0, `x/crypto` v0.58.0, and
  frpc **0.71.1 built from source** instead of the downloaded release binary (the
  downloaded 0.71.0 carried 26 reachable findings from its older Go and `x/*`
  dependencies; the source build has none). govulncheck: our code 0 reachable apart
  from the two accepted moby findings below; frpc 0 apart from the stripping false
  positive.
- **Go standard-library CVEs** (the bulk of our findings — `crypto/tls`, `crypto/x509`,
  `net/url`, `net/http`, …) are fixed by the **Go toolchain**. `src/go.mod`'s `go` line
  and the builder `Dockerfile` (`golang:1.27.3-alpine`) pin it; CI's `setup-go`
  reads `go.mod`. Bump the toolchain as new ones are disclosed — this is not a one-time
  fix. On **2026-09-11** the pin moved `go1.26.4` → `go1.27.1`, clearing 6 stdlib CVEs
  (`net/url`, `crypto/tls` ×2, `net/http` ×2, `encoding/asn1`) that had been disclosed
  since the previous bump.
- **`golang.org/x` module CVEs** — bumped **2026-09-11**: `x/net` v0.55.0 → **v0.59.0**
  (GO-2026-5942), `x/crypto` v0.53.0 → **v0.57.0** (GO-2026-6355, -6354, -6303), and
  `x/text` v0.38.0 → **v0.42.0** (GO-2026-5970, the only one that was *reachable*).
  Together with the toolchain bump this took govulncheck from **9 reachable / 8 module**
  findings to **2 reachable / 4 module** — and every remaining one has no fix available
  (see below).
- **frpc CVEs** — remediated in `frp/go.mod`: bump `x/net`/`x/crypto` there at once, or
  frp itself (then also `FRP_VERSION` in the `Justfile` and `src/embedded/frpc.go`, and
  the tunnel repository's server version).

### Accepted findings (not actionable)

As of 2026-09-11 **every remaining govulncheck finding reports `Fixed in: N/A`** — there
is nothing left that a dependency bump can clear. Five are in `github.com/docker/docker`
(2 reachable, 3 module-only) and one is the unmaintained `golang.org/x/crypto/openpgp`
(GO-2026-5932, no fix version, we never call it). `v28.5.2+incompatible` is already the
**latest** published `docker/docker`, so there is no newer release to move to.

`govulncheck` reports two CVEs in `github.com/docker/docker` as reachable; they are
**accepted / not applicable**:

- **GO-2026-4887** (AuthZ-plugin bypass) and **GO-2026-4883** (plugin-privilege off-by-one)
  are **daemon-side** Moby issues. reagent is a Docker **client** — it manages containers and
  never runs an authorization plugin or installs plugins, so the vulnerable code paths are not
  exercised. govulncheck flags them only because importing the client SDK pulls the module's
  `init()` chain into the call graph.
- There is **no fix in `github.com/docker/docker`** (all versions affected, incl. v28.x); the
  fix lives only in the renamed, still-**beta** module `github.com/moby/moby/v2` (as of
  2026-09-11: `v2.0.0-beta.23`, still no stable release).
  Migrating a production agent to a beta Docker SDK isn't warranted for a non-applicable issue.
- **Do not trust trivy's fix hint here.** trivy reports `github.com/docker/docker
  v28.5.2+incompatible → 29.3.1` as fixable. That `29.3.1` is the *Docker product*
  version; there is **no v29 on the `github.com/docker/docker` module path** (the proxy
  list ends at `v28.5.2+incompatible`), because v29 ships as `github.com/moby/moby/v2`.
  `go get github.com/docker/docker@v29.3.1` cannot resolve. Same accepted reasoning.

Revisit when `github.com/moby/moby/v2` reaches a stable release (then migrate the SDK import).

## Roadmap

1. ~~Report-only → gating~~ — blocking since 2026-10-22.
2. Migrate the Docker SDK to `github.com/moby/moby/v2` once it is stable; then drop the
   two moby VEX statements.

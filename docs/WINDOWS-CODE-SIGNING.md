# Windows code signing (two-tier self-signed PKI)

The Windows agent and its frpc binary are Authenticode-signed so that:

- the service installer can import a trusted publisher (UAC shows
  "IronFlock", and WDAC/AppLocker signer rules become possible), and
- the agent can **pin** self-update downloads to our key on-device
  (`src/codesign`), independent of whatever CAs the machine trusts.

Signing does **not** silence first-run SmartScreen (self-signed accrues no
reputation) and does **not** clear frp's antivirus PUA classification — that is
handled operationally (see the Defender exclusion / WDAC rule in
`WINDOWS-SERVICE-CHECKLIST.md`). This is about trust of *our* binaries on
devices we administer, at zero certificate cost.

## Why two tiers

A single self-signed cert imported to Trusted Root is a hazard: a leaked CI key
could vouch for arbitrary code on every installed device, un-revocably (the
installer only runs at install), and rotating the cert strands the fleet.

So:

- **Root CA** — key generated once, kept **OFFLINE, never in CI**. Its public
  cert is embedded (`src/codesign/roots/*.crt`) for on-device pinning and
  imported to each device's Trusted Root store by the installer. More than one
  root can be embedded at a time to bridge a root rotation (see below).
- **Leaf** — signs binaries in release CI; its PFX lives in the
  `WINDOWS_SIGNING_PFX_B64` GitHub Actions secret. Rotate/revoke the leaf freely
  under the same root without touching any device.

## One-time setup

Generate the PKI (validated commands):

```bash
# 1. Root CA — keep root.key OFFLINE (hardware token / offline vault).
openssl req -x509 -newkey rsa:4096 -keyout root.key -out root.crt -days 3650 -nodes \
  -subj "/CN=IronFlock Code Signing Root/O=IronFlock" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -addext "basicConstraints=critical,CA:TRUE"

# 2. Leaf key + CSR, signed by the root with the codeSigning EKU.
openssl req -newkey rsa:2048 -keyout leaf.key -out leaf.csr -nodes \
  -subj "/CN=IronFlock Device Agent/O=IronFlock"
openssl x509 -req -in leaf.csr -CA root.crt -CAkey root.key -CAcreateserial \
  -out leaf.crt -days 825 \
  -extfile <(printf "keyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\n")

# 3. Bundle leaf + root into a PFX for osslsigncode (choose a strong password).
openssl pkcs12 -export -out leaf.pfx -inkey leaf.key -in leaf.crt -certfile root.crt \
  -passout pass:CHANGEME

# 4. Verify.
openssl verify -CAfile root.crt leaf.crt                 # -> leaf.crt: OK
openssl x509 -in leaf.crt -noout -ext extendedKeyUsage   # -> Code Signing
```

Then wire it up:

1. Put the root's public cert at `src/codesign/roots/ironflock-root.crt` (PEM).
   When `roots/` holds no real cert, on-device verification is a safe no-op.
2. Add repo/org GitHub Actions secrets:
   - `WINDOWS_SIGNING_PFX_B64` = `base64 -w0 leaf.pfx`
   - `WINDOWS_SIGNING_PFX_PASSWORD` = the PFX password.
3. Ship one release. It signs `device-agent.exe` + `frpc.exe`
   (`scripts/sign-windows.sh`, osslsigncode + free RFC3161 timestamp) and the
   installer imports `root.crt`.
4. After a full signed release cycle, flip enforcement to fatal (PR-6) so
   unsigned/wrong-signer downloads and self-updates are **rejected** on-device.
   Either set `enforce = true` in `src/codesign/codesign.go`, or build the
   cutover release with `-ldflags "-X reagent/codesign.enforceStr=true"`.
   `Enforcing()` still requires a configured root, so it can never reject
   everything for lack of a pin.

## Leaf rotation (routine — every ~2 years, or on leaf-key leak)

Issue a new leaf from the **same** root (setup steps 2–3), update the two
secrets. Devices keep trusting the root, so already-installed agents accept the
new leaf's signatures with no reinstall. The root key never touches CI, so a CI
compromise only exposes the leaf, which this rotation replaces.

## Root rotation (rare — before the 10-year root expiry, or on root-key leak)

On-device verification pins to the root(s) embedded under
`src/codesign/roots/*.crt`, and Go's chain check fails once a root's own
validity lapses — so the root **must** be rotated before it expires. Because
self-updates are pinned, the new root reaches devices via an **overlap window**:
the fleet trusts both roots at once, bridged by a transition release. The agent
accepts a signer that chains to **any** embedded root, so:

1. **Transition release.** Generate a new root (setup step 1) and add its public
   cert to `src/codesign/roots/` **alongside** the current one. Keep signing
   with a leaf under the **old** root (unchanged secrets), so the
   currently-running agents — which trust only the old root — accept this update
   as normal. After it lands, devices embed + trust both roots and the installer
   imports both.
2. **Cutover release.** Issue a leaf under the **new** root and update the two
   secrets. Agents past the transition release (trusting both) accept it. Wait
   until fleet telemetry shows everyone is past the transition release.
3. **Cleanup release.** Delete the **old** root's `.crt` from
   `src/codesign/roots/`. Uninstall/reinstall removes it from device trust
   stores; a plain self-update stops trusting it. Signed under the new root.

### Root-key compromise is the hard case

If the **root private key** leaks, you cannot cleanly evict it through the
update channel — the channel's trust depends on it, and an attacker with the key
can sign updates the fleet accepts (including ones that re-pin to their own
root). Recovery is out-of-band: manual reinstall / re-provisioning per device.
This is inherent to pinned self-update, and is exactly why the root key is
generated offline and never in CI. Treat the offline-root discipline as
load-bearing, not ceremony.

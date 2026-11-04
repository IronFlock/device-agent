# Windows code signing (two-tier private PKI)

The Windows agent and its frpc binary are Authenticode-signed so that:

- the agent **refuses** a downloaded update or frpc that IronFlock did not
  sign (`src/codesign`), independent of whatever CAs the machine trusts, and
- the service installer can import a trusted publisher (UAC shows
  "IronFlock", and WDAC/AppLocker signer rules become possible).

This is in addition to the Ed25519 update signature (`src/updatesig`), which
every agent download needs on every platform; Authenticode is the
Windows-native second check on the same bytes.

Signing does **not** silence first-run SmartScreen (a private root accrues no
reputation) and does **not** clear frp's antivirus PUA classification; that is
handled operationally (see the Defender exclusion / WDAC rule in
`WINDOWS-SERVICE-CHECKLIST.md`). This is about trust of *our* binaries on
devices we administer, at zero certificate cost.

## The two tiers

- **Root CA** "IronFlock Code Signing Root" (RSA 4096, valid to 2036-07-07).
  Its key signs only signer certificates; no release step uses it. Its public certificate
  is embedded (`src/codesign/roots/*.crt`) for on-device pinning and imported
  into each device's Trusted Root store by the installer. More than one root
  can be embedded at a time to bridge a root rotation (below).
- **Signer** "IronFlock Device Agent": the Cloud KMS key
  `ironflock-release/agent-codesign` (RSA 4096, `europe-west1`, project
  `record-1283`), so the private key cannot be exported or copied. Its
  certificate, issued by the root, is committed at
  `src/codesign/signer/agent-codesign.crt`. Only the identity `agent-signer`
  may sign with it (`roles/cloudkms.signer` on that key alone), and only from a
  release run of `release.yml` on `main` (workload identity attribute
  `release`).

Until 2026-11 the signer was a PFX in the GitHub Actions secret
`WINDOWS_SIGNING_PFX_B64`; binaries signed with it keep verifying for as long
as that certificate is valid, because it chains to the same root.

## What the agent checks

`codesign.Verify` (Windows; used by the self-update swap and every agent and
frpc download) makes two independent checks, and both must pass:

1. **WinVerifyTrust**: the signature is well-formed and matches the file.
   A root the machine does not trust (`CERT_E_UNTRUSTEDROOT`) or does not
   have at all (`CERT_E_CHAINING`) is accepted here; the machine trust store
   is not the trust decision. The second is the answer on every machine
   without our root in its store, because the signature does not carry the
   root (see Signing): devices installed before the installer imported the
   root, which a self-update never adds, and the CI runner that checks the
   published binaries (`just codesign-check-published`). Until 1.5.5 only the
   first was accepted, so such devices log `self-check FAILED` on 1.5.4 and
   would have refused every update once enforcement was on.
2. **The pin** (`verifyFile`, Go, the same on every platform): the file has
   exactly one signature, at its end; the signature has exactly one signer,
   and exactly one carried certificate is that signer; that certificate's key
   made the signature; the digests match the file; and that certificate chains
   to an embedded root, with no other trust store. A certificate a signature
   merely *carries* proves nothing: anyone can add our public signer
   certificate to their own signature.

Enforcement is **on** since 1.5.6 (`enforce = true`): a download that fails
is deleted and not run, and an update that fails is not activated. 1.5.4 and
1.5.5 only logged a failure and installed the file anyway. The certificate is
checked at the time of the check, not at signing time, so **a signer
certificate must be replaced before it expires**; the release refuses to
publish with one that has less than 30 days left. The device's clock matters
for the same reason: a clock outside the signer certificate's validity fails
the check, and such a device refuses updates until its clock is right.

At every start the agent also checks its own executable in the background
(`codesign.ReportSelf`) and logs `codesign: self-check passed` or
`codesign: self-check FAILED`, with the result of each of the two checks, how
long WinVerifyTrust took, and the signer. Nothing is refused because of it,
but a device that logs `FAILED` refuses updates signed like the agent it runs:
it is a device to look at by hand.

### How enforcement was turned on

A device whose check fails once enforcement is on refuses every later update,
including the one that would fix it; only a reinstall by hand recovers it. So
enforcement came in three steps, and the same steps apply whenever the check
itself changes in a way that could refuse more:

1. Release with enforcement off, and let the Windows devices update to it,
   including devices that reach only an appliance (no internet). This was
   1.5.4, and 1.5.5 with the `CERT_E_CHAINING` fix above.
2. Read the agent logs of those devices: every one must log
   `codesign: self-check passed`, and no download may log `signature
   verification failed … (proceeding: enforcement off)`. On 2026-11-04 the
   only Windows device in the field (behind an appliance) did, on 1.5.5.
3. Then set `enforce = true` in `src/codesign/codesign.go`
   (`TestShippedBuildEnforces` pins it). The self-update and download tests in
   `src/selfupdate` and `src/system` stand in for the check (their test
   binaries are unsigned); `TestDownloadAuthenticode` and the
   `TestActivate…Signature…` tests cover what the agent does with its answer.

Turning enforcement off again takes a release that the devices still accept,
signed like the current one: a device that already refuses updates cannot be
reached by a release that switches it off.

The same check runs in three places before devices see a binary, with the
agent's own code (`sign-update authenticode`, `just codesign-check`):

- `release.yml`, right after signing, on the bytes about to be published (a
  dry run checks against its throwaway root);
- `scripts/sign-release.sh`, on the published bytes, before it signs them for
  promotion: an unsigned or foreign-signed Windows binary never goes live;
- CI on every pull request (`windows-unit`): the published agent and frpc pass
  the check of the changed code, WinVerifyTrust included.

## Signing

`scripts/sign-windows.sh release <files>` (release CI) signs with jsign
(pinned version and SHA-256) through Cloud KMS, with the committed signer
certificate and the root as the chain and an RFC 3161 timestamp
(DigiCert, Sectigo as fallback). jsign 7.6 puts only the signer certificate
into the signature and leaves the self-signed root out, with either kind of
key store; the root comes from the pin (agents) or from the machine's store
(Windows). Signatures made with the earlier PFX signer, such as frpc 0.71.1's,
still carry the root. `scripts/sign-windows.sh dry-run <files>`
(release pull requests) makes a throwaway root and signer and runs the same
jsign call against a PKCS #12 file.

## Setting up the signer (once; again for a new signer)

Steps 1–2 change Google Cloud; step 3 needs the root key.

1. The KMS key and who may use it:

   ```bash
   gcloud kms keys create agent-codesign --project record-1283 --location europe-west1 --keyring ironflock-release --purpose asymmetric-signing --default-algorithm rsa-sign-pkcs1-4096-sha256 --protection-level software
   gcloud kms keys add-iam-policy-binding agent-codesign --project record-1283 --location europe-west1 --keyring ironflock-release --member serviceAccount:agent-signer@record-1283.iam.gserviceaccount.com --role roles/cloudkms.signer
   ```

   jsign needs nothing else: with the key version in the alias and the
   certificate given, its only KMS call is `asymmetricSign`.

2. Its public key:

   ```bash
   gcloud kms keys versions get-public-key 1 --project record-1283 --location europe-west1 --keyring ironflock-release --key agent-codesign --output-file agent-codesign.pub.pem
   ```

3. The certificate, issued by the root from that public key alone (OpenSSL 3.4
   or later, for `-not_before`/`-not_after`; no request needs signing):

   ```bash
   mkdir -p src/codesign/signer
   printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n' > agent-codesign.ext
   openssl x509 -new -force_pubkey agent-codesign.pub.pem -subj "/CN=IronFlock Device Agent/O=IronFlock" -CA src/codesign/roots/ironflock-root.crt -CAkey root.key -set_serial "0x$(openssl rand -hex 16)" -not_before 20260710110831Z -not_after 20360607000000Z -extfile agent-codesign.ext -out src/codesign/signer/agent-codesign.crt
   openssl verify -CAfile src/codesign/roots/ironflock-root.crt src/codesign/signer/agent-codesign.crt
   ```

   The certificate starts with the root (2026-07-10), so a device whose clock
   runs behind accepts it as it accepted the earlier signer, and ends a month
   before the root. A long lifetime is acceptable here because the key cannot
   leave KMS; replace the certificate (or the key) before it ends.

4. Commit the certificate and release. The release pull request's dry run
   exercises the signing path; the release signs through KMS and checks the
   result before publishing.
5. After that release, delete the GitHub Actions secrets
   `WINDOWS_SIGNING_PFX_B64` and `WINDOWS_SIGNING_PFX_PASSWORD`.

## Replacing the signer (routine, or at once if it is misused)

Issue a new certificate from the **same** root (a new KMS key version, or a
new key: steps 1–3, `KMS_KEY_VERSION` in `scripts/sign-windows.sh`), commit
it, release. Devices keep trusting the root, so installed agents accept the
new signer with no reinstall. To stop a misused signer, disable its KMS key
version at once; the next release signs with the new one.

## Root rotation (rare: before the root expires in 2036, or if its key leaks)

The agent pins to the root(s) embedded under `src/codesign/roots/*.crt`, and
the chain check fails once a root's own validity ends, so the root **must** be
rotated before it expires. Because self-updates are pinned, the new root
reaches devices through an **overlap**: the agent accepts a signer that chains
to **any** embedded root, so:

1. **Transition release.** Generate a new root and add its public
   certificate to `src/codesign/roots/` **alongside** the current one. Keep
   signing with the signer under the **old** root, so agents that trust only
   the old root accept this update. After it lands, devices trust both roots
   and the installer imports both.
2. **Cutover release.** Issue a signer certificate under the **new** root
   (setup step 3) and commit it. Agents past the transition release accept it.
   Wait until the fleet is past the transition release first.
3. **Cleanup release.** Delete the **old** root's `.crt` from
   `src/codesign/roots/`. Uninstall and reinstall removes it from device trust
   stores; a plain self-update stops trusting it.

The public runbook for all IronFlock signing keys is the website page
`/docs/security/signing-keys`.

### Root-key compromise is the hard case

If the **root private key** leaks, you cannot cleanly evict it through the
update channel: an attacker with the key can issue a signer the fleet accepts.
The Ed25519 update signature still stands in the way (it is a separate key,
held in Cloud KMS and OpenBao), but recovery of the Authenticode trust is out
of band: manual reinstall or re-provisioning per device. This is why the root
key's protection is load-bearing, not ceremony.

# Pinned code-signing roots

Every `*.crt` in this directory is a trusted code-signing **root** the agent
pins to on-device (`codesign`), and that the Windows installer imports into the
device trust stores. A binary is accepted if its signer chains to **any** root
here.

Having more than one root is how the **root is rotated** without a hard cutover
(see `docs/WINDOWS-CODE-SIGNING.md`):

1. Transition release — add the NEW root's public cert here alongside the old
   one, but keep signing releases with a leaf under the OLD root. Devices then
   trust both roots.
2. Cutover release — start signing with a leaf under the NEW root; devices past
   the transition release accept it.
3. Cleanup release — delete the OLD root's `.crt` here.

Only **public** certificates belong here, never a private key. This README keeps the directory
non-empty so the `//go:embed` never fails if all roots are temporarily removed
(verification then no-ops, safe for the pre-signing transition).

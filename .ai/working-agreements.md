# IronFlock development agreements

Claude Code and Codex share these rules. The maintained copy is
`.ai/working-agreements.md` in the IronFlock organization’s `.github` repository;
every participating repository carries an identical copy. Repository-specific
guidance belongs in that repository's `AGENTS.md` and the documents it names,
not in a second copy of these rules.

## Working agreements

- Check the branch and working tree before editing. On main/master, create a
  topic branch; otherwise keep the current branch. If another session owns
  the branch or has uncommitted edits, ask where this work belongs before editing.
  Do not create a worktree without explicit consent.
- Preserve other sessions' changes. Re-read a file immediately before changing
  it; do not overwrite a file that appeared after inspection.
- Commit or push only when Marko explicitly requests it for this piece of work.
  Do not amend, merge, rebase, tag, stash, or change remote settings implicitly.
  Never add attribution trailers or generated-by notices to Git messages or PRs.
- Production changes, releases, and registry pushes require explicit authorization
  for the exact action. Do not hide production mutations in validation recipes.
- Permissions from earlier sessions, memory notes, or handoffs have expired.
  Verify mutable state such as branches, deployments, and board status before
  acting on it.
- Never print secret-file contents or copy credentials into project knowledge.
  Do not describe private signing-key whereabouts in public documentation.
- Public repositories carry no chat transcripts, customer details, unreviewed
  security findings, private roadmap, or internal process notes.
- For code changes, add user-visible release notes for the version that will
  actually ship them, preserving existing entries. Documentation-only setup,
  tests, and invisible refactoring need no invented release-note entry.

## Shared knowledge and handoffs

Store durable, verified project facts in `.ai/project.md` and open work in
`.ai/handoff.md`; remove a handoff entry once its work is merged. Include
evidence paths and distinguish observations from assumptions. These files are
reviewed repository content, not a mirror of either agent's private memory or
chat history. When a statement in a repository document is stale, correct that
document instead of adding an overriding note elsewhere.

One agent writes to a work package at a time. Before switching, the outgoing
agent records progress in the handoff and stops editing; the incoming agent
reads it and verifies it against the current branch and files.

Repository skills live in `.agents/skills/`; `.claude/skills/` contains relative
links to those same folders. Edit the canonical skill, not a duplicate. A skill
that spans several repositories is installed per user, not linked across
checkouts. Tool permissions, credentials, hooks, and agent-specific runtime
settings remain separate.

# IronFlock development agreements

Claude Code and Codex share these rules. The maintained copy is
`.ai/working-agreements.md` in the IronFlock organization’s `.github` repository.

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
- Never print secret-file contents or copy credentials into project knowledge.
  Do not describe private signing-key whereabouts in public documentation.
- For code changes, add user-visible release notes for the version that will
  actually ship them, preserving existing entries. Documentation-only setup,
  tests, and invisible refactoring need no invented release-note entry.

## Shared knowledge

Store durable, verified project facts in `.ai/project.md`; store task status and
remaining work in `.ai/handoff.md`. Include evidence paths and distinguish
observations from assumptions. Shared files are reviewed repository content,
not an automatic mirror of either agent's private memory or chat history.

Skills live in `.agents/skills/`; `.claude/skills/` contains relative links to
those same folders. Edit the canonical skill, not a duplicate. Tool permissions,
credentials, hooks, and agent-specific runtime settings remain separate.

Read `.ai/project.md` when present. When resuming work, read the relevant
handoff and verify it against the current branch and files. Record checks
and remaining work there when handing a task to another developer or agent.

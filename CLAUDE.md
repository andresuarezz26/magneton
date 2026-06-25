# magneton — repo conventions

Greenfield Go project. Autonomous Android **ticket → PR** agent. See
`.context/attachments/jIQXSc/plan-handoff.md` for the binding spec and decisions.

## Layout
- `main.go` — entrypoint.
- `cmd/` — Cobra CLI (`agent` binary): `init`, `run`, `logs`, and Phase-2 stubs (`start`/`stop`/`status`).
- `internal/config` — `~/.agent/config.toml` loader (Decision 15).
- `internal/secrets` — OS keychain + env fallback (Decision 14).
- `internal/jira` — Jira Cloud read + comment.
- `internal/git` — worktree-per-ticket + branch mgmt (Decision 7).
- `internal/agent` — drives `claude -p` stream-json + report.json contract (Decisions 3, 6).
- `internal/build` — Gradle compile/test gate.
- `internal/vcs` — `gh` PR + templated PR body / Jira comment (Decision 10).
- `internal/paths` — `~/.agent` layout.

## Build & run
```
go build -o agent .
./agent init          # scaffold ~/.agent/config.toml
./agent run PROJ-123  # one ticket → one PR
```

## Phasing
- **Phase 1 (current):** `agent run <TICKET>` — thin end-to-end, no daemon.
- **Phase 2:** SQLite state, daemon poll loop, concurrency, `agent status` (Decisions 2, 8, 9).
- **Phase 3:** interactive `agent init` wizard, GoReleaser + Homebrew (Decisions 13, 17).

## Conventions
- Keep packages small and dependency-light; shell out to `git`/`gh`/`claude` rather than wrapping SDKs.
- The agent edits code **and verifies it** — it discovers + runs the project's own build/tests (handling per-project setups and company build skills) and self-certifies via `report.json` `verified`. The **orchestrator** owns commit, push, and PR, and trusts that verdict (it no longer runs Gradle itself).
- Never auto-merge — stop at `review`.

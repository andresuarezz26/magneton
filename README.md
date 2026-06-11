# droidpilot

Autonomous **Android ticket → PR** agent. A local tool that turns a Jira ticket
into a review-ready pull request: it provisions an isolated git worktree, drives
a headless [Claude Code](https://claude.com/claude-code) session to make the
change, gates the result on a Gradle compile + unit-test run, and opens a PR for
a human to review and merge. It never auto-merges.

> Built to the binding spec in `.context/attachments/jIQXSc/plan-handoff.md`.
> Decision numbers below (e.g. *Decision 7*) refer to that document.

## Status

- **Phase 1 — complete:** `agent run <TICKET>` — the thin end-to-end "one ticket → one PR" loop, including bounded self-correct retries.
- **Phase 2 — complete:** background daemon (`agent start`/`stop`), SQLite state with atomic claiming, concurrency-capped fleet, `agent status` (+ `--watch`), Jira JQL polling + transition-on-claim, desktop notifications.
- **Phase 3 — planned:** interactive `agent init` wizard, GoReleaser + Homebrew distribution.

## How it works (Phase 1 loop)

```
fetch ticket → isolated worktree (ai/<ticket>-<slug>)
            → drive claude -p (stream-json) to edit code
            → read .agent/report.json completion contract
            → gate: gradle compile + unit tests
                 ├─ fail → feed errors back into the SAME session, retry (≤ max_retries)
                 └─ pass → commit → push → open PR → comment on ticket → stop at `review`
```

**Design principle:** the agent only *edits code and writes a report*; the
orchestrator owns build, commit, push, and PR. That separation is what makes the
gate and the self-correct loop trustworthy rather than self-reported.

## Project layout

| Path | Responsibility | Decision |
|---|---|---|
| `main.go` | entrypoint | — |
| `cmd/` | Cobra CLI (`agent`): `init`, `run`, `logs`, `status`/`start`/`stop` | 7, 8 |
| `internal/config` | `~/.agent/config.toml` loader | 15 |
| `internal/secrets` | OS keychain (`security`) + `$DROIDPILOT_*` env fallback | 14 |
| `internal/jira` | Jira Cloud read + comment (Phase 2: search + transition) | 5 |
| `internal/git` | worktree-per-ticket + branch + push | 7 |
| `internal/agent` | drive `claude -p` stream-json + `report.json` contract | 3, 6 |
| `internal/build` | Gradle compile/test gate (per-phase steps) | — |
| `internal/vcs` | `gh` PR + templated PR body / Jira comment | 10 |
| `internal/runner` | per-ticket pipeline shared by CLI + daemon | 4, 9 |
| `internal/store` | SQLite state, atomic claim, status queries | 2 |
| `internal/daemon` | poll loop + concurrency-capped worker pool | 5, 8 |
| `internal/notify` | desktop notification + daemon-log line | 11 |
| `internal/paths` | `~/.agent` layout | — |

## Install / build

Requires Go 1.24+, `git`, the `gh` CLI (for PRs), and an authenticated `claude` CLI.

```bash
go build -o agent .
```

## Configuration

`agent init` scaffolds `~/.agent/config.toml` and the templates:

```toml
jira_base_url = "https://your-org.atlassian.net"
jira_email    = "you@your-org.com"
poll_interval = 30          # seconds (Phase 2 daemon)
concurrency   = 3           # max concurrent sessions (Phase 2 daemon)
max_budget_usd = 5          # per-session cost cap passed to claude

[[repo]]
path        = "~/src/android-app"
jql         = "labels = ai-agent AND status = 'To Do'"
branch      = "ai/{ticket}-{slug}"
compile     = "./gradlew :app:compileDebug"
test        = "./gradlew testDebugUnitTest"
max_retries = 3
# base      = "main"        # base branch; auto-detected if omitted
```

**Secrets** (Decision 14) — env vars win, otherwise the macOS keychain:

```bash
export DROIDPILOT_JIRA_TOKEN=...   # Jira API token (paired with jira_email)
gh auth login                      # used by `gh pr create`
# optional: export DROIDPILOT_ANTHROPIC_TOKEN=...  (else uses your logged-in claude)
```

## Usage

```bash
agent init                 # scaffold config + templates
agent run PROJ-123         # one ticket → one PR (records to the state DB)
agent run PROJ-123 --dry-run   # everything except push + PR (safe first run)
agent logs PROJ-123        # print the session log

# Test mode — no Jira required (see below):
agent run LOCAL-1 --local --title "..." --desc "..." --dry-run

# Unattended fleet (Phase 2):
agent start                # poll Jira + run the fleet (foreground; background with: agent start &)
agent status               # aligned, grep-able table of every session
agent status --watch       # live-refreshing view
agent stop                 # graceful shutdown (drains in-flight sessions)
```

The daemon polls each repo's `jql` every `poll_interval` seconds, atomically
claims new tickets (so none is processed twice), transitions them to *In
Progress*, runs up to `concurrency` sessions at once, and fires a desktop
notification when a ticket reaches `review` or `needs-you`.

Runtime data lives under `~/.agent/`: `config.toml`, `worktrees/<ticket>/`,
`logs/<ticket>.log`, `templates/`, and (Phase 2) `state.db`.

### Safety rails (Decision 16)

- Sessions run with a scoped `--allowed-tools` allowlist; writes happen in the
  worktree (the session's cwd).
- `--dry-run` performs edits + build but never pushes or opens a PR.
- `max_budget_usd` caps per-session cost.
- **Known MVP limitation:** the `Bash` tool is currently allowed broadly, so a
  session *can* read outside its worktree. Tighten `allowed_tools` (e.g.
  `Bash(./gradlew:*)`) before unattended use on a sensitive repo.

## How to test

### 1. Automated (no setup)

```bash
go build -o agent .
go vet ./...
go test ./...
```

### 2. Full loop, no Jira / no cloud (`--local`)

Runs the complete pipeline (worktree → real Claude session → gate → self-correct
→ commit) against a throwaway repo. Copy/paste:

```bash
go build -o agent .

ROOT=$(mktemp -d); ORIGIN="$ROOT/origin.git"; WORK="$ROOT/app"
git init -q --bare "$ORIGIN"; git init -q "$WORK"
git -C "$WORK" config user.email t@t.co; git -C "$WORK" config user.name T
echo "# app" > "$WORK/README.md"; git -C "$WORK" add -A; git -C "$WORK" commit -qm init
git -C "$WORK" branch -M main; git -C "$WORK" remote add origin "$ORIGIN"
git -C "$WORK" push -qu origin main

export DROIDPILOT_HOME="$ROOT/.agent"; mkdir -p "$DROIDPILOT_HOME"
cat > "$DROIDPILOT_HOME/config.toml" <<EOF
jira_base_url = ""
[[repo]]
path    = "$WORK"
branch  = "ai/{ticket}-{slug}"
compile = "true"
test    = 'test "\$(cat hello.txt 2>/dev/null)" = "hello world"'
EOF

./agent run HELLO-1 --local \
  --title "Create hello.txt containing exactly: hello world" --dry-run
```

Expected: `… → gate green ✓ → committed on ai/hello-1-…`. Inspect with
`git -C "$DROIDPILOT_HOME/worktrees/HELLO-1" log --oneline` and
`cat "$DROIDPILOT_HOME/worktrees/HELLO-1/hello.txt"`.

**Test the self-correct + `needs-you` path:** set `test = "false"` and
`max_retries = 2` in the config above and re-run. You'll see it retry into the
same Claude session and route to `needs-you` after exhausting retries.

> `DROIDPILOT_HOME` overrides the `~/.agent` location — use it to keep tests
> isolated from your real config.

### 3. Real Jira + real repo

Edit `~/.agent/config.toml` with your Jira URL/email and a real Android
`[[repo]]`, set `DROIDPILOT_JIRA_TOKEN`, run `gh auth login`, then:

```bash
./agent run PROJ-123 --dry-run     # inspect the worktree first
./agent run PROJ-123               # opens a real PR
```

## Roadmap

1. **Local laptop** (now) — unsupervised single + concurrent runs on your machine.
2. **Shared team runners** — always-on, warm caches, central visibility.
3. **Hosted platform** — managed execution + emulator/device gating, RBAC, audit.

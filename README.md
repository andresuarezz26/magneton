# magneton

**Android development workflow automation.** magneton turns Android tickets into
reviewed PRs autonomously, and its home is a **terminal dashboard (TUI)**: run
`agent` with no arguments and you get a live view of every agent plus one-key
actions to start, watch, unblock, and ship work — without leaving the screen.

Under the hood, for each ticket it provisions an isolated git worktree, drives a
headless [Claude Code](https://claude.com/claude-code) session to plan and
implement the change, manages Android emulators when the task needs instrumented
tests, gates the result on a Gradle compile + test run, and opens a PR for a human
to review and merge. **It never auto-merges.** It handles the whole loop — not
just "write code" — including keeping you in the loop when it gets stuck, moving
Jira tickets through their status workflow, and coordinating emulator resources
across concurrent tasks.

The TUI is the friendly front; every action is also a plain subcommand
(`agent run`, `agent doctor`, …) for scripts and CI.

## Status

- **Phase 1 — complete:** `agent run <TICKET>` — the thin end-to-end loop, including bounded self-correct retries.
- **Phase 2 — complete:** background daemon (`agent start`/`stop`), SQLite state with atomic claiming, concurrency-capped fleet, `agent status` (+ `--watch`), Jira JQL polling, Jira status transitions, desktop notifications.
- **Phase 3 — complete:** interactive `agent init` wizard with connectivity check, `--version`, GoReleaser + Homebrew packaging.
- **TUI hub — complete:** bare `agent` opens a live dashboard that is the home base for everything — start/answer/resume/stop agents, open the worktree in Android Studio or resume the session in Claude Code, run doctor, edit config, and control the daemon, all from one screen.

## How it works

```
fetch ticket
  → move Jira ticket to In Progress
  → create isolated worktree (ai/<ticket>-<slug>)
  → write local.properties with Android SDK path
  → plan stage: Claude reads codebase + ticket, writes plan.json
       • sets needs_emulator=true for UI/instrumentation tasks
       • posts plan to Jira (with blockers/questions if any)
       ↓
       if questions → post to Jira, wait for user to update ticket, re-run
  → [if needs_emulator] boot AVD in background goroutine
  → implement stage: Claude edits code
       (emulator boots in parallel — saves 60–120s wall time)
  → gate: gradle compile
       ├─ needs_emulator=false → ./gradlew testDebugUnitTest
       └─ needs_emulator=true  → [wait emulator ready] → ./gradlew connectedDebugAndroidTest
       ├─ fail → feed errors back into the SAME Claude session, retry (≤ max_retries)
       │         after max_retries: comment on Jira ticket asking for human help
       └─ pass → commit → push → open PR → comment on Jira ticket → stop at `review`
```

**Design principle:** the agent only *edits code and writes a report*; the
orchestrator owns build, commit, push, PR, and Jira status. That separation is
what makes the gate and the self-correct loop trustworthy rather than self-reported.

## Deterministic by design

Everything except *writing code* is plain, deterministic orchestration — Go shelling
out to `git`/`gh` or calling the Jira REST API directly. The AI is never asked to
create a branch, push, open a PR, or move a Jira ticket. That buys three things:

- **Trustworthy results.** The model physically *cannot* push or open a PR, so it
  can't self-report success. Only the orchestrator pushes, and only after the real
  Gradle gate passes — so "green" means the code actually compiled and tested, not
  that the agent said it did.
- **No hallucination risk on plumbing.** Branch names, PR creation, and Jira
  transitions are exact API/CLI calls. They can't be malformed, invented, or
  "almost right" the way generated shell commands can.
- **Fewer tokens, faster runs.** No round-trips spent asking the model to run `git`
  or `gh`. The model spends its budget on the one thing only it can do — the code.

How the boundary is enforced (two layers, no MCP anywhere):

| Operation | Owner | Mechanism |
|---|---|---|
| Worktree + branch | orchestrator | `git worktree add -b …` (`internal/git`) |
| Commit / push | orchestrator | `git add -A && git commit`, `git push` (`internal/git`) |
| Open PR | orchestrator | `gh pr create …` (`internal/vcs`) |
| Jira fetch / comment / transition | orchestrator | Jira REST API over HTTP (`internal/jira`) |
| Edit source, run tests | agent | `claude -p` with a scoped `--allowed-tools` allowlist |

The agent runs under a per-stage `--allowed-tools` allowlist: read-only at plan and
review, and at implement only `Edit`/`Write`/`Read`/search, `Bash(./gradlew:*)`, and
**read-only** git (`status`/`diff`/`log`/`show`). No `git push`, no `git commit`, no
`gh`, no network — so a tool call outside that set is rejected, not just discouraged.
The implement allowlist is configurable via `allowed_tools`; widening it to include
`gh` or `git push` would hand those steps back to the model and is not recommended.

## Project layout

| Path | Responsibility |
|---|---|
| `main.go` | entrypoint |
| `cmd/` | Cobra CLI (`agent`): `init`, `run`, `doctor`, `logs`, `status`/`start`/`stop` |
| `internal/config` | `~/.agent/config.toml` loader |
| `internal/secrets` | OS keychain (`security`) + `$DROIDPILOT_*` env fallback |
| `internal/jira` | Jira Cloud read, comment, status transitions |
| `internal/git` | worktree-per-ticket + branch + push |
| `internal/agent` | drive `claude -p` stream-json + `plan.json`/`report.json` contract |
| `internal/build` | Gradle compile/test gate + Android emulator lifecycle |
| `internal/vcs` | `gh` PR + templated PR body / Jira comment |
| `internal/runner` | per-ticket pipeline shared by CLI + daemon |
| `internal/store` | SQLite state: atomic claiming + emulator resource coordination |
| `internal/daemon` | poll loop + concurrency-capped worker pool + idle emulator shutdown |
| `internal/notify` | desktop notification + daemon-log line |
| `internal/paths` | `~/.agent` layout + `local.properties` writer |

## Install

### Prerequisites

**Required** — this is the whole list to get a reviewed diff:

- **`git`** — in PATH
- **`claude`** — Claude Code CLI, *authenticated* (a logged-in session is enough; it's the engine)
- **Go 1.24+** — to build from source (or install via [Homebrew](#homebrew-release))
- **A repo that builds** — magneton runs *your* repo's build/test (e.g. Gradle). For a quick smoke test you can set the compile/test commands to `true`.

**Optional** — add only what you actually use:

- **`gh`** (authenticated) — only to **open the pull request**. `agent run … --dry-run` skips push + PR, so you can run the whole loop without it.
- **Jira** (site URL + email + API token) — only to **fetch tickets automatically** by key (`agent run KAN-4`). For `.md` tickets, Jira is never touched.
- **Android SDK** — only to build/test an actual Android project.
- **An AVD + `adb`/emulator** — only for **instrumented (on-device UI) tests**. Without one, instrumented tasks fall back to unit tests.
- **`ANTHROPIC_API_KEY`** — only if you're *not* using a logged-in `claude` session.

### From source

```bash
git clone https://github.com/andresuarezz26/magneton
cd magneton
make install          # builds and installs to ~/.local/bin/agent
```

Or build without installing:
```bash
make build            # → ./agent binary in the current directory
```

### Homebrew (release)

```bash
brew install magneton/tap/magneton
```

Maintainers cut releases with GoReleaser (`make snapshot` for a local dry run);
`.goreleaser.yaml` builds static binaries for darwin/linux amd64/arm64 and
publishes the Homebrew formula.

## Quickstart — your first PR from a `.md` file (no Jira, no emulator)

You don't need Jira or an emulator to start. Describe the work in a plain markdown
file and point magneton at it — it plans, implements, runs your build/test gate,
and (unless `--dry-run`) opens a PR.

```bash
# 1. One-time: point magneton at your repo + how to build it.
#    `agent init` asks for Jira too, but you can leave those blank — only the
#    repo path and compile/test commands matter for local .md runs.
agent init

# 2. Write a ticket as markdown (the first # heading is the title).
cat > add-logout.md <<'EOF'
# Add a logout button to the settings screen

Wire it to AuthRepository.logout() and navigate back to the login screen.
EOF

# 3. Run it. --dry-run does everything except push + PR (no `gh` needed).
agent run ./add-logout.md --dry-run

# 4. See what it produced.
git -C ~/.agent/worktrees/ADD-LOGOUT diff
```

Drop `--dry-run` (with `gh` authenticated and a pushable `origin`) to open the PR.
Run several at once: `agent run a.md b.md c.md`. Then open the dashboard with
`agent` to watch, answer, resume, or stop them.

**Want it to pull tickets for you instead?** Set up Jira (below) and run by key:
`agent run KAN-123`. **Need real on-device UI tests?** Add an AVD (see
[Android Emulator](#android-emulator)). Both are optional.

## Setup

Run **`agent`** (the hub) and pick **Setup wizard** from the menu (`:`), or run
**`agent init`** directly. On a terminal it launches an interactive wizard (prompts
for Jira URL/email, repo path, build/test commands, and tokens — stored in the
OS keychain — then runs a connectivity check). When stdin isn't a TTY (CI), it
scaffolds a commented `~/.agent/config.toml` to edit by hand. Once configured, just
run **`agent`** and start a ticket from the dashboard.

> **Only the repo path and compile/test commands are required.** Leave the Jira
> fields blank to run from `.md` files; fill them in only when you want magneton to
> fetch tickets automatically by key (the connectivity check will simply mark Jira
> as not-configured, which is fine).

To **edit the config** later:
```bash
open -t ~/.agent/config.toml        # macOS — opens in the default text editor
# or, if you have $EDITOR set:
"${EDITOR:-vi}" ~/.agent/config.toml
```

> Note: plain `open ~/.agent/config.toml` fails on a stock macOS with
> `kLSApplicationNotFoundErr` — no app is registered for the `.toml` extension.
> Use `open -t` (default *text* editor) or pass an explicit editor.

To **check that everything is connected** after editing:
```bash
agent doctor
```
This shows the config path, tests Jira/git/claude/gh connectivity, and verifies
the Android SDK + AVD setup. No prompts — safe to run at any time.

## Configuration

The config file lives at `~/.agent/config.toml`. A full example:

```toml
jira_base_url            = "https://your-org.atlassian.net"
jira_email               = "you@your-org.com"
jira_in_progress_status  = "In Progress"   # status name in your Jira board (default: "In Progress")
poll_interval            = 30              # seconds between daemon polls
concurrency              = 3              # max concurrent sessions (daemon)
max_budget_usd           = 5             # per-session Claude cost cap

# Android emulator — optional, used automatically for UI/instrumentation tasks
avd_name              = "Pixel_6_API_34"        # from `emulator -list-avds`
android_sdk_path      = "~/Library/Android/sdk" # auto-detected from $ANDROID_HOME if omitted
emulator_idle_timeout = 30                       # minutes of idle before emulator shuts down

[[repo]]
path           = "~/src/android-app"
jql            = "labels = ai-agent AND status = 'To Do'"
branch         = "ai/{ticket}-{slug}"
compile        = "./gradlew :app:compileDebug"
test           = "./gradlew testDebugUnitTest"
connected_test = "./gradlew connectedDebugAndroidTest"  # used when emulator is needed
max_retries    = 3
# base          = "main"   # base branch; auto-detected if omitted
```

### Jira board status names

The `jira_in_progress_status` field must match the **exact status name** in your
Jira board (case-insensitive). If your board uses a different language, set it
accordingly:

```toml
jira_in_progress_status = "En progreso"   # Spanish example
```

Run `agent doctor` after changing this — if the transition fails, the doctor
output will list the available status names for your board.

### Secrets

Secrets are stored in the macOS keychain (set during `agent init`) or as
environment variables:

```bash
export DROIDPILOT_JIRA_TOKEN=...          # Jira API token (paired with jira_email)
gh auth login                             # used by `gh pr create`
# optional:
export DROIDPILOT_ANTHROPIC_TOKEN=...    # if not using the logged-in claude session
```

## Android Emulator (optional)

You only need this for **instrumented (on-device UI) tests**. If you don't set an
`avd_name`, magneton never boots an emulator — instrumented tasks fall back to unit
tests. Set it up only when you want Espresso/Compose tests to run on a device.

The orchestrator decides **automatically** during the plan stage whether a task
needs an emulator. Claude inspects the ticket and codebase:

- `needs_emulator=true` — task involves UI tests, Espresso, Compose instrumented tests, or creates/modifies files under `androidTest/`
- `needs_emulator=false` — domain layer, use cases, repositories, ViewModels, or unit tests under `test/`

You don't configure this — it's a per-ticket decision made at plan time.

### Emulator lifecycle

1. **Boot in parallel.** When a task needs the emulator, boot starts immediately after the plan stage — in parallel with the implement stage. By the time Claude finishes writing code, the emulator is usually already warm.
2. **Shared resource.** The emulator is coordinated via SQLite. If two concurrent tasks both need it, one runs tests while the other waits — no two processes start simultaneously.
3. **Already running?** If an emulator is already connected (e.g., left open from Android Studio), it is reused without restarting.
4. **Idle shutdown.** The emulator stays warm between tasks and shuts down automatically after `emulator_idle_timeout` minutes of inactivity, or when `agent stop` is called.

To inspect the emulator's current state:
```bash
adb devices        # shows connected emulators
agent doctor       # shows emulator/AVD status
```

To open the worktree for a ticket in Android Studio (to inspect what the agent changed):
```bash
open -a "Android Studio" ~/.agent/worktrees/<TICKET>
```

### Prerequisites

```bash
# Create an AVD via Android Studio's AVD Manager, or via command line:
avdmanager create avd -n Pixel_6_API_34 \
  -k "system-images;android-34;google_apis;arm64-v8a"

# Verify:
emulator -list-avds    # should list Pixel_6_API_34
adb devices            # shows connected devices/emulators
```

Then set in `~/.agent/config.toml`:
```toml
avd_name         = "Pixel_6_API_34"
android_sdk_path = "~/Library/Android/sdk"
```

## Usage

### The hub (the default — TUI-first)

Run **`agent`** with no arguments and you land in the hub: a live dashboard that is
the home base for everything. (`agent monitor`/`agent top` open the same screen.)

```
magneton · 6 agents · 2 need you      20:49:28  ·  ● daemon pid 41021

   ＋ Start new ticket(s)          ← first row; press enter to start

 ▾ NEEDS YOU (2)
   ▮ KAN-6   awaiting-answer  Improve content discovery…        5m
   ✗ KAN-4   failed           Create integration tests…         7m
 ▾ RUNNING (1)
   ● KAN-7   working          Add a logout button              12s
 ▾ DONE (1)
   ✓ KAN-5   review           Mock use case for home            7d

 ↑↓ select · enter: choose · : commands · q: quit
```

- **Triage at a glance** — agents grouped **NEEDS YOU / STOPPED / RUNNING / DONE**,
  with live state, the ticket title, and age. STOPPED is detected from the real
  process (a dead pid), not a guess.
- **Start work** — the top row is *Start new ticket(s)*; press enter and type one
  or more Jira keys or `.md` paths to launch them (in parallel).
- **Act on an agent** — select it, press enter, and pick from its menu:
  - **Answer the questions** — type your answer; it's written back and the agent resumes.
  - **Resume (verify & ship)** — after you fix the worktree by hand, re-run the gate on your changes and open the PR.
  - **Open Android Studio** — open the worktree as a project.
  - **Open in Claude Code** — a new terminal resumes the agent's own Claude session in the worktree.
  - **Stop & clean up** — kill the process and remove the worktree.
- **Everything else** — the same menu (or `:`) reaches **Doctor**, **Edit config**,
  the **Setup wizard**, and **Start/Stop daemon**. The header shows daemon status live.

Every action maps to a subcommand, so the same work is scriptable for CI:

```bash
agent init                     # scaffold config + run connectivity check
agent doctor                   # check config path + connectivity (no prompts)

agent run PROJ-123             # run one Jira ticket end-to-end
agent run PROJ-123 --dry-run   # everything except push + PR (safe first run)
agent run PROJ-123 --resume    # after a manual fix: re-gate the existing worktree, then PR
agent logs PROJ-123            # print the session log

# No Jira required — point it at local markdown files:
agent run ticket.md                        # one local ticket, no Jira
agent run feat-a.md feat-b.md feat-c.md    # several at once, in parallel

# Unattended fleet:
agent start                    # poll Jira and run sessions (foreground)
agent start --once             # poll one cycle, run claimed tickets, then exit
agent status                   # aligned table of every session
agent status --watch           # live-refreshing view
agent stop                     # graceful shutdown (drains in-flight sessions)
```

### Local files instead of Jira

You don't need Jira to use magneton. Pass one or more file paths to `agent run`
and each is treated as a ticket:

```bash
agent run ./tickets/add-logout-button.md ./tickets/fix-crash.md
```

- **Title + body.** The first markdown `# H1` is the ticket summary (or the first
  non-blank line if there's no H1); everything after it is the description handed
  to the agent.
- **Ticket id.** Derived from the filename: `add-logout-button.md` → `ADD-LOGOUT-BUTTON`,
  used for the worktree, branch, log file, and `agent status`. Same-basename files
  in one run are disambiguated with a `-2`/`-3` suffix.
- **Parallelism.** Multiple args run concurrently, capped at `concurrency` from your
  config (default 3). Each ticket gets its own worktree, branch, and `<id>.log`;
  terminal lines are prefixed `[<id>]`. If one fails, the others still run and the
  command exits non-zero.
- **Plan + questions.** With no Jira to comment on, the agent's plan and any blocking
  questions print to the terminal and the per-ticket log instead. Answer by editing
  the `.md` and re-running (same stop-and-re-run flow as Jira).

You can also mix Jira keys and files in one invocation: `agent run PROJ-1 todo.md`.

### The plan + questions workflow

Before implementing, magneton posts a plan comment on the Jira ticket. If Claude
has questions or blockers, the comment explains what's needed:

> 🤖 *magneton has questions before starting [PROJ-123]*
>
> *Please update the ticket description* with your answers, then re-run:
> `agent run PROJ-123`

**What to do:** edit the Jira ticket **description** to answer the questions (don't
reply in comments — magneton reads the description). Then re-run the command shown.

### When the agent gets stuck

If the build gate fails after all retries (state `needs-you`/`failed`), magneton
comments on the Jira ticket with the error details and the worktree path so you
can investigate:

```bash
open -a "Android Studio" ~/.agent/worktrees/PROJ-123
```

**Fix it by hand, then resume** — magneton keeps *your* changes, re-runs the gate
on them, and (if green) commits + opens the PR. It does **not** re-plan or let the
agent touch your fix:

```bash
agent run PROJ-123 --resume
```

(In the TUI: select the ticket, `o` to open the worktree, fix it, then `R` to
resume.) A plain `agent run PROJ-123` (no `--resume`) starts over from scratch and
**discards** uncommitted worktree changes — use `--resume` to keep a manual fix.

### Safety rails

- Sessions run with a scoped `--allowed-tools` allowlist; all writes happen inside the worktree.
- `--dry-run` performs edits + build but never pushes or opens a PR.
- `max_budget_usd` caps per-session Claude cost.
- The agent never auto-merges — it always stops at `review`.

Runtime data lives under `~/.agent/`: `config.toml`, `worktrees/<ticket>/`,
`logs/<ticket>.log`, `templates/`, and `state.db`.

## How to test

### 1. Automated (no setup)

```bash
go build -o agent .
go vet ./...
go test ./...
```

### 2. Full loop, no Jira / no cloud (`--local`)

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

### 3. Real Jira + real repo (live run)

**Prerequisites**
- A Jira Cloud site + project. Create an API token at <https://id.atlassian.com/manage-profile/security/api-tokens>.
- A clone of your Android repo with a pushable `origin` (GitHub) and `gh auth login` done.
- An authenticated `claude` (`claude --version` works).

**1 — Configure:**
```bash
agent init
# Jira base URL  → https://YOURORG.atlassian.net
# Jira email     → you@yourorg.com
# Repository     → /abs/path/to/android-repo
# Compile        → ./gradlew :app:compileDebug
# Test           → ./gradlew testDebugUnitTest
# JQL            → labels = ai-agent AND status = "To Do"
# Jira API token → (stored in keychain)
```

**2 — Seed one safe ticket.** In Jira, create a small, well-scoped chore (e.g.
"bump library X to version Y" or "fix this lint warning"), give it the
`ai-agent` label, leave it in **To Do**.

**3 — Single-ticket dry run first (no PR, no Jira writes):**
```bash
agent run PROJ-123 --dry-run
git -C ~/.agent/worktrees/PROJ-123 diff
agent logs PROJ-123
```

**4 — Real single ticket (opens a PR + comments on the ticket):**
```bash
agent run PROJ-123
```

**5 — Exercise the daemon for one cycle:**
```bash
agent start --once     # claims matching tickets, runs them, then exits
agent status
```

**6 — Leave it running unattended:**
```bash
agent start &
agent status --watch
agent stop
```

> Tip: keep `concurrency` low (1–2) and `max_budget_usd` modest for the first
> real runs, and start with small, well-scoped tickets.

## Roadmap

1. **Local laptop** (now) — unsupervised single + concurrent runs on your machine.
2. **Shared team runners** — always-on, warm caches, central visibility.
3. **Hosted platform** — managed execution + emulator/device fleet, RBAC, audit.

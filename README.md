
<img width="1337" height="669" alt="Screenshot 2026-06-26 at 8 05 10 PM" src="https://github.com/user-attachments/assets/50fc8a4b-e9fb-4baf-ad17-a2cffb217bab" />


# Magneton

**Stop babysitting your coding agent. Start supervising a fleet of them.**

magneton is a TUI for Android devs. Point a few agents at your Jira tickets (or local markdown tickets), watch them work, and only step in when one flags **NEEDS YOU**: a blocking question, a stuck run, or a PR that's ready for your review. You supervise many tickets instead of babysitting one.

Android-native: each agent boots the emulator, runs Gradle, and knows what instrumented tests need. It does one platform for real, not "AI for everything."

```
magneton · 5 agents · 2 need you

▾ NEEDS YOU (2)
  ▮ KAN-2   awaiting-answer   Improve content discovery…     5m
  ✗ KAN-3   failed            Migrate settings to Compose     2m
▾ RUNNING (2)
  ● KAN-5   working           Add a logout button            12s
  ● KAN-8   planning          Fix crash on back navigation     3s
▾ DONE (1)
  ✓ KAN-4   review            Mock use case for home screen    7d
```

5 agents working, you only touch the 2 that need you. Each agent runs a ticket through **plan → implement → verify** and opens a PR. It asks before it guesses, and **never auto-merges**: it stops at review.

---

## Install

### One-paste install (recommended)

Open Claude Code and paste this. Claude does the rest.

> Install magneton: run `git clone --single-branch --depth 1 https://github.com/andresuarezz26/magneton.git ~/.magneton && cd ~/.magneton && ./setup` — then run `magneton init` to configure your repo (path, build commands, optional Jira credentials) and verify connectivity. Make sure `~/.local/bin` is in your PATH.

Claude clones the repo, builds the binary, puts it in `~/.local/bin/magneton`, and walks you through `magneton init`. The whole thing takes under a minute.

### Manual install

**Prerequisites:**

- [Claude Code](https://claude.ai/download) — authenticated and in your PATH (`claude --version` works)
- [Android Studio](https://developer.android.com/studio) — for your Android project and AVD management
- `git` and `gh` (GitHub CLI, authenticated) — for branch management and opening PRs
- Go 1.24+ — to build from source

```bash
git clone https://github.com/andresuarezz26/magneton.git ~/.magneton
cd ~/.magneton
./setup
magneton init
```

`./setup` builds and installs to `~/.local/bin/magneton`. `magneton init` asks for your repo path, build/test commands, optional Jira credentials, and whether to share anonymous usage data.

---

## Quick start

```bash
# Write a ticket as markdown (first # heading is the title).
cat > add-logout.md <<'EOF'
# Add a logout button to the settings screen

Wire it to AuthRepository.logout() and navigate back to the login screen.
EOF

# Run it. --dry-run does everything except push + open a PR.
magneton run ./add-logout.md --dry-run

# Open the dashboard to watch progress and act on any tickets that need you.
magneton
```

Drop `--dry-run` (with `gh` authenticated) to open a real PR. Run several tickets at once:

```bash
magneton run a.md b.md c.md
```

### Jira integration

If you connect Jira during `magneton init` (site URL + email + API token), you can run tickets directly by their Jira key — magneton fetches the title and description automatically:

```bash
magneton run PROJ-123          # fetch from Jira, plan → implement → verify → PR
magneton run PROJ-123 PROJ-124 # run two Jira tickets in parallel
```

No Jira? No problem — the markdown file approach above works without it.

---

## Dashboard

Run `magneton` with no arguments to open the live TUI hub.

```
magneton · 4 agents · 1 need you      20:49:28  ·  ● daemon pid 41021

  ＋ Start new ticket(s)
  ⚙  Edit config

▾ NEEDS YOU (1)
  ▮ KAN-6   awaiting-answer  Improve content discovery…       5m
▾ RUNNING (2)
  ● KAN-7   working          Add a logout button              12s
  ● KAN-8   planning         Fix crash on back navigation      3s
▾ DONE (1)
  ✓ KAN-5   review           Mock use case for home screen     7d

  ↑↓ select · enter: choose · : commands · q: quit
```

Select a ticket and press enter to answer a question, resume after a manual fix, open the worktree in Android Studio, or stop the agent. The top two rows open a new ticket or edit the config.

---

## CLI reference

```bash
magneton                          # open the TUI dashboard (default)

# Local markdown tickets (no Jira required)
magneton run ./ticket.md          # plan → implement → verify → PR
magneton run a.md b.md c.md       # run several in parallel

# Jira tickets (requires Jira configured in magneton init)
magneton run PROJ-123             # fetch Jira ticket, then plan → implement → verify → PR
magneton run PROJ-123 PROJ-124    # run two Jira tickets in parallel

# Flags (work with both Jira keys and markdown files)
magneton run PROJ-123 --dry-run   # skip push + PR (safe for first runs)
magneton run PROJ-123 --resume    # re-gate a worktree you fixed by hand, then PR

# Other commands
magneton doctor                   # connectivity check (Jira, git, claude, gh)
magneton logs PROJ-123            # print the session log
magneton status                   # table of all sessions
magneton start                    # start the background daemon
magneton stop                     # stop the daemon gracefully
```

---

## Configuration

Config lives at `~/.agent/config.toml`. Created by `magneton init`, editable any time.

```toml
jira_base_url  = "https://your-org.atlassian.net"
jira_email     = "you@your-org.com"
concurrency    = 3           # max parallel tickets
max_budget_usd = 5           # per-session Claude cost cap

[[repo]]
path    = "~/src/android-app"
branch  = "ai/{ticket}-{slug}"
compile = "./gradlew :app:compileDebug"
test    = "./gradlew testDebugUnitTest"
```

Secrets (Jira API token, Anthropic API key) are stored in the macOS keychain during `magneton init`, or as environment variables:

```bash
export DROIDPILOT_JIRA_TOKEN=...
export DROIDPILOT_ANTHROPIC_TOKEN=...   # optional — only if not using a logged-in claude session
```

Run `magneton doctor` after any config change to verify connectivity.

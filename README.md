<img width="1100" height="679" alt="magneton" src="https://github.com/user-attachments/assets/f27ac84a-4fba-44f8-bd2f-891d474a6a9d" />


# Magneton

**An autonomous Android development pipeline. Drop in a Jira ticket. Get back a pull request.**

Magneton runs the full engineering cycle on its own: reads your ticket, writes a plan, implements the change, compiles the project, runs the tests (including instrumented tests on an emulator when needed), and opens a pull request — all without you watching. You come back to review code, not manage agents.

Run one ticket or a dozen at once. Each gets its own git worktree and runs in parallel. A shared emulator semaphore makes sure no two agents fight over the same device. When a ticket is clean, you get a PR. When one gets stuck, it flags you and waits.

This is not an AI assistant you drive. It is a pipeline that turns your backlog into reviewed, mergeable pull requests.

---

## What it does

Each ticket moves through three stages autonomously:

- **Plan** — reads your codebase, writes a focused implementation plan, identifies any blocking questions, and decides upfront whether the ticket requires instrumented tests (Compose, Espresso, `androidTest/`) or unit tests only.
- **Implement** — follows the approved plan and makes the minimal change it describes.
- **Verify** — discovers how your project builds, compiles it with Gradle, and runs the appropriate tests. If the plan flagged a device dependency, it boots the emulator and runs instrumented tests too. The ticket is marked green only after the build and tests pass.

When a ticket passes verification, the agent opens a pull request. When it hits something it cannot resolve — a compile error beyond its reach, an ambiguous requirement — it flips to **NEEDS YOU** in the TUI. You answer the question or open the worktree in Android Studio, then let it continue.

---

## Why this exists

At my company, management started measuring developer productivity by pull requests merged and lines of code added. Flawed metrics, but the ones I'm judged on. I started running Claude Code in parallel with git worktrees, but I was babysitting every agent — constant context switching across terminals, manually creating branches, driving plan mode one session at a time, coordinating the emulator by hand, writing PR descriptions.

The code was never the bottleneck. The toil around it was. So I built Magneton to own that toil: one dashboard for all tickets, automatic worktree and branch management, staged plan → implement → verify execution, emulator scheduling, and PR creation. My PR count roughly doubled. The only thing I still do is review the output.

---

## How it compares

**[Firebender](https://firebender.com/)** is an Android Studio plugin — a context-aware assistant that lives inside your IDE. You prompt it, review its suggestions, and guide it. Great tool; different category. Magneton runs unattended from a ticket description and hands you a PR.

**[OpenHands](https://www.openhands.dev/) / [Devin](https://devin.ai/)** are cloud-based autonomous agents that work across any codebase. They have no awareness of Gradle build systems, Android emulator lifecycle, instrumented vs. unit test routing, or Compose-specific verification. Magneton is built around those constraints specifically.

**tmux + Claude Code** works, but you manage state yourself — no single view of what every agent is doing, no automatic worktree setup, no emulator coordination, no PR automation.

Magneton's edge is specificity: it knows what Android projects look like, how they build, and what it means to actually verify an Android change.

---

## Quick start

**Prerequisites:**
- [Claude Code](https://claude.ai/download) — authenticated and in your PATH (`claude --version` works)
- [Android Studio](https://developer.android.com/studio) — for your Android project and AVD management
- `git` and `gh` (GitHub CLI, authenticated)

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/andresuarezz26/magneton/main/install.sh | bash
```

Downloads the right pre-built binary for your platform (macOS arm64/x86, Linux), installs to `~/.local/bin/magneton`, and checks your prerequisites. No Go required.

Then configure your repo:

```bash
magneton init
```

**Want to build from source?** Clone the repo, run `./setup` (requires Go 1.24+), then `magneton init`.

---

## Running tickets

Type `magneton` to open the TUI dashboard. Select **Start new ticket(s)** and pick how to add the ticket:

- **Paste ticket content** — copy from Jira, Linear, a doc, anywhere. Magneton extracts the ticket ID for branch naming and asks you to confirm it. You can drag screenshots into the terminal to attach them; the agent sees them during planning.
- **From Jira** — enter the ticket key and Magneton fetches the title and description directly.
- **From a .md file** — point at a local markdown file.

Queue several tickets at once; they stack as chips in the input, then press Enter to launch them all. The dashboard shows live status and logs for every ticket. When one finishes, its PR link appears. When one needs you, it flags itself and waits.

---

## How it works under the hood

Magneton is a Go program that drives a deterministic pipeline. Each ticket runs in its own goroutine with an isolated git worktree, shelling out to Claude Code at each stage. State lives in a local SQLite database. The TUI polls it every second.

The emulator is a shared resource: a SQLite-backed semaphore lets only one agent hold it at a time while others queue. No two agents fight over the same device.

Model routing is configurable per stage — you can run a faster model for planning and a more capable one for implementation.

---

## Cost

Magneton uses your existing Claude Code subscription or API key. No separate account, no markup. Each ticket consumes tokens like a full Claude Code working session on that task. Running five tickets in parallel is roughly five concurrent sessions worth of usage. Start with one ticket to calibrate cost before scaling up.

---

## CLI reference

```bash
magneton                          # open the TUI dashboard

magneton run PROJ-123             # fetch Jira ticket → plan → implement → verify → PR
magneton run PROJ-123 PROJ-124    # run two tickets in parallel
magneton run ./ticket.md          # run from a local markdown file

magneton run PROJ-123 --dry-run          # skip push and PR (safe for first runs)
magneton run PROJ-123 --resume           # re-gate a worktree you fixed by hand, then PR
magneton run PROJ-123 --ship             # skip verification: commit + push + PR from your manual fix
magneton run PROJ-124 --base ai/proj-123 # stack on another branch; PR targets it

magneton doctor                   # connectivity check: Jira, git, claude, gh
magneton logs PROJ-123            # print the session log
magneton status                   # table of all sessions
magneton start                    # start the background daemon
magneton stop                     # stop the daemon
```

---

## Configuration

Select **Edit config** in the TUI, or edit `~/.agent/config.toml` directly. You can configure the model used at each stage, your repo path, branch naming conventions, and Jira credentials.

<img width="1015" height="400" alt="Screenshot 2026-06-26 at 8 26 30 PM" src="https://github.com/user-attachments/assets/a4b0261a-eb2e-4001-a231-1e047dfaeeb8" />

Run `magneton doctor` after any config change to verify connectivity.

---

## Jira setup (optional)

Magneton can pull a ticket's title and description straight from Jira so you can run by key (`magneton run PROJ-123`) instead of writing a markdown file.

1. **Create an API token** at [id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens).

2. **Enter it during `magneton init`:**

   | Prompt | Example |
   | --- | --- |
   | Jira base URL | `https://your-org.atlassian.net` |
   | Jira email | `you@your-org.com` |
   | Jira API token | `ATATT3xFfGF0...` |

   The token is stored in your OS keychain, not the config file. For CI use, set `MAGNETON_JIRA_TOKEN` instead.

3. **Verify** with `magneton doctor`.

---

## License

MIT. See [LICENSE](LICENSE).

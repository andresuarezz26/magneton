<img width="1100" height="679" alt="magneton" src="https://github.com/user-attachments/assets/f27ac84a-4fba-44f8-bd2f-891d474a6a9d" />


# Magneton

**A terminal UI for running multiple Claude Code agents on your Android tickets in parallel.**

Start one or more tickets from the TUI by pasting the path to a local markdown file or a Jira ticket ID, and each runs through a plan → implement → verify loop in its own git worktree, in parallel. When a ticket passes, the agent opens a pull request and you review and test it. When an agent needs you, it flips to **NEEDS YOU**: if it asked a question, you answer it right in the TUI; if it's stuck on something like a compile error it can't resolve, you can resume its Claude Code session or open the worktree in Android Studio to fix it by hand.

---

## Motivation

At my company, to prove AI was actually improving developer productivity, management started measuring us by pull requests merged and lines of code added. Flawed metrics, but they're the ones I'm judged on. So I started running Claude Code in parallel with git worktrees to push my numbers up, but I ended up babysitting every agent, and the context switching fried my brain even at 2-3 tickets at a time.

Most of the work wasn't the code, it was the toil around it: switching between terminals to supervise each agent, creating a branch and worktree per ticket, driving plan mode, coordinating a single emulator across runs, asking Claude to compile and run the unit tests, opening the right worktree in Android Studio, picking a model per stage, and writing the PR description. So I built Magneton to keep all of it in one terminal and automate the repetitive parts. Now, if a ticket is well defined, I rarely touch it. I just catch issues in the PR. My numbers roughly doubled once I started using it.

### What about alternatives? 

I tried [conductor.build](https://conductor.build), but you still have to actively drive your dev workflow, and it's a closed-source desktop app, while I wanted something open-source I could run in the terminal. I also wanted a tool that runs on its own and pings me only when it gets stuck.

I've also used tmux, and I've seen people make it work, but you still have to learn shortcuts, manage a lot of terminal windows, and keep track of each agent's state yourself, since there's no single dashboard showing what every agent is doing.


## Quick start

1. Type `magneton` in the terminal to open the TUI.

2. Select "Start new ticket(s)".

3. Paste the path of the local markdown ticket, or the Jira ticket ID if you set up the integration. You can run multiple tickets at once. Just separate them with a space.

4. The dashboard shows each ticket's IN-PROGRESS status and live logs of what the agent is doing.

5. When the agent finishes, the ticket moves to DONE / review with a PR opened, ready for you to approve.

---

## How it works

Under the hood it's a Go program that drives a deterministic git/worktree flow. Each ticket runs in its own goroutine that moves through plan → implement → verify in sequence (with a self-review pass before it certifies), shelling out to a Claude Code session at each stage. State lives in a local SQLite database, and the TUI polls it every second to refresh status.

For Android specifically, the emulator is a shared resource: a SQLite-backed semaphore lets only one agent hold the emulator at a time to run the app or instrumentation tests while the others wait their turn. No two agents fight over the same device.

## Cost

Magneton runs on your existing Claude Code setup. It shells out to the `claude` CLI you already have authenticated, using your own Claude subscription or API key. There is no separate Magneton account, no API key to add, and no markup.

What that means for your bill: each ticket is a full Claude Code session that plans, edits, and verifies the change end to end, so it consumes tokens like a real working session on that task, not a one-shot prompt. Running tickets in parallel multiplies that. Five tickets at once is roughly five concurrent Claude Code sessions worth of usage. On a Pro/Max subscription it counts against those usage limits; on the API it is metered like any other Claude Code work. Start with one ticket to get a feel for the cost before you fan out.

---

## Install

### One-paste install (recommended)


**Prerequisites:**
- [Claude Code](https://claude.ai/download) — authenticated and in your PATH (`claude --version` works)
- [Android Studio](https://developer.android.com/studio) — for your Android project and AVD management
- `git` and `gh` (GitHub CLI, authenticated) — for branch management and opening PRs

Open Claude Code and paste this. Claude does the rest.

> Install magneton: run `git clone --single-branch --depth 1 https://github.com/andresuarezz26/magneton.git ~/.magneton && cd ~/.magneton && ./setup` — then run `magneton init` to configure your repo (path, build commands, optional Jira credentials) and verify connectivity. Make sure `~/.local/bin` is in your PATH.

Claude clones the repo, builds the binary, puts it in `~/.local/bin/magneton`, and walks you through `magneton init`. The whole thing takes under a minute.

### Manual install

**Additional Prerequisites:**
- Go 1.24+ — to build from source

```bash
git clone https://github.com/andresuarezz26/magneton.git ~/.magneton
cd ~/.magneton
./setup
magneton init
```

`./setup` builds and installs to `~/.local/bin/magneton`. `magneton init` asks for your repo path, build/test commands, optional Jira credentials, and whether to share anonymous usage data.

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

1. Select "Edit config".

2. You can configure the models used by each stage, the repo url, branch naming convention and Jira setup.
<img width="1015" height="400" alt="Screenshot 2026-06-26 at 8 26 30 PM" src="https://github.com/user-attachments/assets/a4b0261a-eb2e-4001-a231-1e047dfaeeb8" />
Config lives at `~/.agent/config.toml`. Created by `magneton init`, editable any time.

3. Run `magneton doctor` after any config change to verify connectivity.

---

## Jira setup (optional)

Magneton can pull a ticket's title and description straight from Jira, so you can run a ticket by its key (`magneton run PROJ-123`) instead of writing a markdown file. To enable it you need a Jira API token.

**1. Create an API token.** Go to [id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens), click **Create API token**, give it a label (for example `magneton`), and copy the value. Atlassian shows it only once.

**2. Enter the values during `magneton init`.** When the wizard reaches the Jira section, fill in:

| Prompt | What to enter | Example |
| --- | --- | --- |
| Jira base URL | Your Atlassian site URL | `https://your-org.atlassian.net` |
| Jira email | The email of your Atlassian account | `you@your-org.com` |
| Jira API token | The token you just created | `ATATT3xFfGF0...` |

The token is stored in your OS keychain, not in the config file. For headless or CI use you can set the `MAGNETON_JIRA_TOKEN` environment variable instead.

**3. Verify.** Run `magneton doctor`. It authenticates against your site, and a passing Jira check means you can now run tickets by key.

---

## License

MIT. See [LICENSE](LICENSE).

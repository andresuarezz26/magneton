<img width="1337" height="669" alt="Screenshot 2026-06-26 at 8 05 10 PM" src="https://github.com/user-attachments/assets/50fc8a4b-e9fb-4baf-ad17-a2cffb217bab" />


# Magneton

**Stop babysitting one Claude Code agent on your Android app. Start supervising a fleet of them.**

Magneton is a TUI for Android devs using Claude Code. Kick off your tickets (local markdown tickets or Jira, both supported), and each ticket runs through a plan → implement → verify process. You only step in when one flags **NEEDS YOU**: a blocking question, or a compilation error or failing unit test the agent cannot figure out on its own. Each agent runs in parallel on its own worktree, and from the TUI you can resume its Claude Code session or open it directly in Android Studio.

---

## Quick start

1. Type `magneton` in the terminal to open the TUI.

2. Select "Start new ticket(s)".

<img width="1225" height="561" alt="Screenshot 2026-06-26 at 8 22 18 PM" src="https://github.com/user-attachments/assets/120e35d4-f186-49f6-adea-489ca7cab23d" />

3. Paste the path of the local markdown ticket, or the Jira ticket ID if you set up the integration. You can run multiple tickets at once. Just separate them with a space.

<img width="992" height="233" alt="Screenshot 2026-06-26 at 8 24 16 PM" src="https://github.com/user-attachments/assets/7baf45bc-1846-4529-9a22-e1c9d8dd6d74" />

4. The dashboard shows the ticket's IN-PROGRESS status while the agent works.

5. When the agent finishes, the ticket moves to DONE / review with a PR opened, ready for you to approve.

---

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

Select "Edit config".
<img width="1039" height="117" alt="Screenshot 2026-06-26 at 8 27 34 PM" src="https://github.com/user-attachments/assets/d401c322-c616-4000-8e42-01ae01355087" />

You can configure the models used by each stage, the repo url, branch naming convention and Jira setup.
<img width="1015" height="400" alt="Screenshot 2026-06-26 at 8 26 30 PM" src="https://github.com/user-attachments/assets/a4b0261a-eb2e-4001-a231-1e047dfaeeb8" />
Config lives at `~/.agent/config.toml`. Created by `magneton init`, editable any time.

Run `magneton doctor` after any config change to verify connectivity.

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

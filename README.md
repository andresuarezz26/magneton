<img width="1100" height="679" alt="magneton" src="https://github.com/user-attachments/assets/f27ac84a-4fba-44f8-bd2f-891d474a6a9d" />

# Magneton

> **Coding agents that don't need supervision. Drop in a ticket. Review the PR.**

```bash
curl -fsSL https://raw.githubusercontent.com/andresuarezz26/magneton/main/install.sh | bash
```

Magneton is an autonomous ticket → PR pipeline for Android. Each ticket runs plan → implement → verify in its own git worktree, in parallel, and opens a pull request only after the agent has actually seen the build and tests pass. If verification fails, the agent fixes and re-runs until it's green - or hands the ticket back to you. You don't prompt the agent - you review its PR like a colleague's.

**Requires:** [Claude Code](https://claude.ai/download) (authenticated), `git` + `gh`, [Android Studio](https://developer.android.com/studio).

## The pipeline

```mermaid
flowchart LR
    A[Ticket] --> B[Plan<br/>read-only]
    B --> C{Blocking<br/>questions?}
    C -->|yes| H[NEEDS YOU<br/>answer in TUI]
    C -->|no| D[Implement<br/>+ self-review]
    D --> E[Verify<br/>build + tests]
    E -->|red| D2[Fix + re-run<br/>until green]
    D2 --> E
    E -->|green| F[Pull Request]
    E -->|stuck| H
    H --> D
```

| Stage | What it does |
|-------|--------------|
| **Plan** | Reads the codebase read-only, writes a focused plan, flags blocking questions, decides if the ticket needs an emulator (Compose/Espresso) or unit tests only |
| **Implement** | Makes the minimal change the plan describes, then adversarially reviews its own diff |
| **Verify** | Discovers how *your* project builds, runs the real build + tests (boots the emulator if needed), certifies green only after seeing them pass |

When an agent gets stuck - ambiguous ticket, compile error it can't fix - the ticket flips to **NEEDS YOU**. Answer in the TUI, resume the Claude session, or open the worktree in Android Studio.

## Why Android-native matters

General ticket→PR agents (Devin, OpenHands, Copilot Workspace) don't know what verifying an Android change means. Magneton does:

- **Emulator as a shared resource** - a SQLite-backed semaphore lets parallel agents take turns on one AVD; no two agents fight over a device
- **Instrumented vs. unit test routing** - decided at plan time, enforced at verify time
- **Gradle-aware verification** - the agent discovers your project's own build setup, including company build scripts
- **Worktree → Android Studio handoff** - one keystroke opens any agent's worktree in the IDE
- **Screenshot tickets** - drag images into the terminal; the agent sees them while planning

## Why I built it

My company measures productivity by PRs merged. I ran Claude Code agents in parallel with git worktrees to keep up, and ended up supervising every one of them - terminals, branches, plan mode, the emulator, PR descriptions. Magneton automates that toil. My PR count roughly doubled; now I mostly just review.

## Usage

```bash
magneton                          # TUI dashboard: queue tickets, watch live status
magneton init                     # configure repo, build commands, optional Jira

magneton run PROJ-123             # one Jira ticket → PR
magneton run a.md b.md c.md       # local markdown tickets, in parallel

magneton run PROJ-123 --dry-run   # skip push + PR (try this first)
magneton run PROJ-123 --resume    # re-verify a worktree you fixed by hand
magneton run PROJ-124 --base ai/proj-123  # stack on another ticket's branch

magneton doctor                   # check Jira, git, claude, gh connectivity
```

In the TUI, add tickets three ways: **paste** the ticket text (from Jira, Linear, anywhere - screenshots attach by drag), enter a **Jira key**, or point at a **.md file**. Queue several, press enter, watch the dashboard.

Config lives at `~/.agent/config.toml` - repo path, per-stage models, branch naming, [Jira credentials](docs/jira.md) (optional; token stored in your OS keychain).

## Cost

Runs on your existing Claude Code subscription or API key - no separate account, no markup. Each ticket is a full working session's worth of tokens; five parallel tickets ≈ five concurrent sessions. Start with one.

## Caveats

- **You still review.** Autonomous loops make autonomous mistakes; the PR gate is where your judgment goes.
- Never auto-merges - every ticket stops at review.
- Well-defined tickets sail through; vague ones come back as questions.

## License

MIT. See [LICENSE](LICENSE).

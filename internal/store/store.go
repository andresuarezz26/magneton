// Package store is the durable session/ticket state (Decision 2).
//
// A single SQLite file gives us transactional "claim-if-unclaimed" semantics so
// a ticket is never processed twice across concurrent workers, survives daemon
// restarts, and makes `agent status` a simple query.
package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go, cgo-free driver
)

// Lifecycle states (Decision 9).
const (
	StateQueued    = "queued"
	StatePlanning  = "planning"
	StateAwaiting  = "awaiting-answer"
	StateWorking   = "working"
	StateReviewing = "reviewing"
	StateBuilding  = "building"
	StateTesting   = "testing"
	StateReview    = "review"
	StateNeedsYou  = "needs-you"
	StateFailed    = "failed"
	// StatePlanReview is the opt-in pause after the plan stage: the plan is ready
	// but the human hasn't approved it yet. It's an idle state (NOT in IsActive)
	// so the driver process has exited and a re-run is allowed, like awaiting-answer.
	StatePlanReview = "plan-review"
	// Terminal post-review states set when the PR is resolved (Decision 7 cleanup).
	StateMerged = "merged"
	StateClosed = "closed"
	// StateStopped is set when a session is manually cancelled from the monitor
	// (process killed + worktree removed).
	StateStopped = "stopped"
)

// IsActive reports whether state means a run is actively in progress (a driver
// process should be alive). Terminal/idle states - review, needs-you, failed,
// merged, closed, stopped, awaiting-answer - return false: the driving process
// has already exited, so its recorded PID is stale and a new run is allowed.
func IsActive(state string) bool {
	switch state {
	case StateQueued, StatePlanning, StateWorking, StateReviewing, StateBuilding, StateTesting:
		return true
	}
	return false
}

// Emulator lifecycle states.
const (
	EmulatorIdle    = "idle"
	EmulatorBooting = "booting"
	EmulatorReady   = "ready"
	EmulatorBusy    = "busy"
)

// Session is one ticket's row.
type Session struct {
	Ticket     string
	Repo       string
	State      string
	Retries    int
	Branch     string
	Worktree   string
	PRURL      string
	Summary    string
	SessionID  string // Claude session ID for cross-stage resume
	PID        int    // OS pid of the process driving this session (0 = unknown)
	SourcePath string // .md file path for local tickets; empty for Jira tickets
	BaseBranch string // stacked-diff base branch name (bare, no origin/ prefix); "" = default
	ShortDesc  string // LLM-generated <10-word gist shown in the dashboard third column
	ReviewPlan bool   // pause after the plan stage so the human can approve/give feedback
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store wraps the SQLite connection.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  ticket      TEXT PRIMARY KEY,
  repo        TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL,
  retries     INTEGER NOT NULL DEFAULT 0,
  branch      TEXT NOT NULL DEFAULT '',
  worktree    TEXT NOT NULL DEFAULT '',
  pr_url      TEXT NOT NULL DEFAULT '',
  summary     TEXT NOT NULL DEFAULT '',
  session_id  TEXT NOT NULL DEFAULT '',
  pid         INTEGER NOT NULL DEFAULT 0,
  source_path TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS emulators (
  avd_name     TEXT PRIMARY KEY,
  state        TEXT NOT NULL DEFAULT 'idle',
  holder       TEXT NOT NULL DEFAULT '',
  pid          INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL
);`

// Open opens (and migrates) the state DB at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migrate existing DBs (ignore "duplicate column" errors on re-run).
	db.Exec(`ALTER TABLE sessions ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN pid INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE sessions ADD COLUMN base_branch TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN short_desc TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN review_plan INTEGER NOT NULL DEFAULT 0`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Claim atomically inserts a queued row for ticket. It returns true if THIS call
// won the claim, false if the ticket was already present (claimed/processed).
func (s *Store) Claim(ticket, repo, summary string) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO sessions (ticket, repo, state, summary, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(ticket) DO NOTHING`,
		ticket, repo, StateQueued, summary, now, now,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetState updates the lifecycle state and retry count.
func (s *Store) SetState(ticket, state string, retries int) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET state=?, retries=?, updated_at=? WHERE ticket=?`,
		state, retries, time.Now().Unix(), ticket,
	)
	return err
}

// SetSessionID persists the Claude session ID for cross-stage resume.
func (s *Store) SetSessionID(ticket, sessionID string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET session_id=?, updated_at=? WHERE ticket=?`,
		sessionID, time.Now().Unix(), ticket,
	)
	return err
}

// SetSourcePath records the .md file path for local (non-Jira) tickets so that
// TUI re-launch actions (Resume, Run again) can reconstruct the right command
// without hitting the Jira API.
func (s *Store) SetSourcePath(ticket, path string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET source_path=?, updated_at=? WHERE ticket=?`,
		path, time.Now().Unix(), ticket,
	)
	return err
}

// SetBaseBranch records the (bare) base branch for a stacked-diff ticket.
func (s *Store) SetBaseBranch(ticket, base string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET base_branch=?, updated_at=? WHERE ticket=?`,
		base, time.Now().Unix(), ticket,
	)
	return err
}

// SetPID records the OS pid of the process driving this session, so the monitor
// can tell a live agent from a dead one (deterministic liveness via kill -0).
func (s *Store) SetPID(ticket string, pid int) error {
	_, err := s.db.Exec(`UPDATE sessions SET pid=? WHERE ticket=?`, pid, ticket)
	return err
}

// SetFields updates branch/worktree/pr_url (any empty string is left unchanged).
func (s *Store) SetFields(ticket, branch, worktree, prURL string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET
		   branch   = CASE WHEN ?='' THEN branch   ELSE ? END,
		   worktree = CASE WHEN ?='' THEN worktree ELSE ? END,
		   pr_url   = CASE WHEN ?='' THEN pr_url   ELSE ? END,
		   updated_at = ?
		 WHERE ticket=?`,
		branch, branch, worktree, worktree, prURL, prURL, time.Now().Unix(), ticket,
	)
	return err
}

// SetShortDesc persists a LLM-generated short description for the dashboard.
// Called at run start (and again when the async LLM upgrade finishes), so stale
// rows from prior failed runs are always refreshed.
func (s *Store) SetShortDesc(ticket, desc string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET short_desc=?, updated_at=? WHERE ticket=?`,
		desc, time.Now().Unix(), ticket,
	)
	return err
}

// SetReviewPlan records whether this ticket should pause after the plan stage.
// Persisted so TUI re-spawns (answer/feedback) keep the gate on.
func (s *Store) SetReviewPlan(ticket string, v bool) error {
	iv := 0
	if v {
		iv = 1
	}
	_, err := s.db.Exec(
		`UPDATE sessions SET review_plan=?, updated_at=? WHERE ticket=?`,
		iv, time.Now().Unix(), ticket,
	)
	return err
}

// Get returns one session.
func (s *Store) Get(ticket string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT ticket, repo, state, retries, branch, worktree, pr_url, summary, session_id, pid, source_path, created_at, updated_at, base_branch, short_desc, review_plan
		 FROM sessions WHERE ticket=?`, ticket)
	return scan(row)
}

// List returns all sessions, most recently updated first.
func (s *Store) List() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT ticket, repo, state, retries, branch, worktree, pr_url, summary, session_id, pid, source_path, created_at, updated_at, base_branch, short_desc, review_plan
		 FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// RegisterEmulator upserts an idle emulator row if not already present.
func (s *Store) RegisterEmulator(avdName string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO emulators (avd_name, state, updated_at)
		 VALUES (?, 'idle', ?)
		 ON CONFLICT(avd_name) DO NOTHING`,
		avdName, now,
	)
	return err
}

// SetEmulatorBooting atomically transitions idle→booting and records the pid.
// Returns true if this caller won the race (only the winner should actually
// run the emulator process).
func (s *Store) SetEmulatorBooting(avdName string, pid int) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE emulators SET state='booting', pid=?, updated_at=? WHERE avd_name=? AND state='idle'`,
		pid, time.Now().Unix(), avdName,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseEmulator sets state=ready, clears holder, and stamps last_used_at.
// Used for both booting→ready and busy→ready transitions.
func (s *Store) ReleaseEmulator(avdName string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`UPDATE emulators SET state='ready', holder='', last_used_at=?, updated_at=? WHERE avd_name=?`,
		now, now, avdName,
	)
	return err
}

// AcquireEmulator atomically claims the emulator for a ticket.
// Returns true only if the row was in state=ready and this caller won.
func (s *Store) AcquireEmulator(avdName, ticket string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE emulators SET state='busy', holder=?, updated_at=? WHERE avd_name=? AND state='ready'`,
		ticket, time.Now().Unix(), avdName,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EmulatorState returns the current state and pid for an AVD.
func (s *Store) EmulatorState(avdName string) (state string, pid int, err error) {
	row := s.db.QueryRow(`SELECT state, pid FROM emulators WHERE avd_name=?`, avdName)
	err = row.Scan(&state, &pid)
	return
}

// SetEmulatorIdle resets state to idle and clears the pid (post-kill).
func (s *Store) SetEmulatorIdle(avdName string) error {
	_, err := s.db.Exec(
		`UPDATE emulators SET state='idle', pid=0, holder='', updated_at=? WHERE avd_name=?`,
		time.Now().Unix(), avdName,
	)
	return err
}

// EmulatorLastUsed returns the last_used_at unix timestamp for an AVD.
func (s *Store) EmulatorLastUsed(avdName string) (int64, error) {
	var t int64
	err := s.db.QueryRow(`SELECT last_used_at FROM emulators WHERE avd_name=?`, avdName).Scan(&t)
	return t, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(r scanner) (*Session, error) {
	var s Session
	var created, updated int64
	var reviewPlan int
	if err := r.Scan(&s.Ticket, &s.Repo, &s.State, &s.Retries, &s.Branch,
		&s.Worktree, &s.PRURL, &s.Summary, &s.SessionID, &s.PID, &s.SourcePath,
		&created, &updated, &s.BaseBranch, &s.ShortDesc, &reviewPlan); err != nil {
		return nil, err
	}
	s.ReviewPlan = reviewPlan != 0
	s.CreatedAt = time.Unix(created, 0)
	s.UpdatedAt = time.Unix(updated, 0)
	return &s, nil
}

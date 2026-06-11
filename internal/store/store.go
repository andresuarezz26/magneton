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
	StateQueued   = "queued"
	StateWorking  = "working"
	StateBuilding = "building"
	StateTesting  = "testing"
	StateReview   = "review"
	StateNeedsYou = "needs-you"
	StateFailed   = "failed"
	// Terminal post-review states set when the PR is resolved (Decision 7 cleanup).
	StateMerged = "merged"
	StateClosed = "closed"
)

// Session is one ticket's row.
type Session struct {
	Ticket    string
	Repo      string
	State     string
	Retries   int
	Branch    string
	Worktree  string
	PRURL     string
	Summary   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store wraps the SQLite connection.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  ticket     TEXT PRIMARY KEY,
  repo       TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL,
  retries    INTEGER NOT NULL DEFAULT 0,
  branch     TEXT NOT NULL DEFAULT '',
  worktree   TEXT NOT NULL DEFAULT '',
  pr_url     TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
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

// Get returns one session.
func (s *Store) Get(ticket string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT ticket, repo, state, retries, branch, worktree, pr_url, summary, created_at, updated_at
		 FROM sessions WHERE ticket=?`, ticket)
	return scan(row)
}

// List returns all sessions, most recently updated first.
func (s *Store) List() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT ticket, repo, state, retries, branch, worktree, pr_url, summary, created_at, updated_at
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

type scanner interface {
	Scan(dest ...any) error
}

func scan(r scanner) (*Session, error) {
	var s Session
	var created, updated int64
	if err := r.Scan(&s.Ticket, &s.Repo, &s.State, &s.Retries, &s.Branch,
		&s.Worktree, &s.PRURL, &s.Summary, &created, &updated); err != nil {
		return nil, err
	}
	s.CreatedAt = time.Unix(created, 0)
	s.UpdatedAt = time.Unix(updated, 0)
	return &s, nil
}

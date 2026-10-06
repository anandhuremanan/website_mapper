package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Names remembers, on disk, the hostnames each subdomain provider listed
// for each domain, and when.
//
// Providers such as crt.sh are slow and often unavailable, while their
// answers change little from week to week. Keeping the last answer means a
// provider's outage does not cost a scan the names it gave before, and a
// restart does not throw away answers that took a minute to get. Over time
// this becomes the server's own small index of the domains people scan.
type Names struct {
	db  *sql.DB
	log *slog.Logger
	// maxAge is how long an answer is kept.
	maxAge time.Duration
}

// namesFile is deliberately not a ".db" file: the scan store treats every
// ".db" file in the data directory as a scan.
const namesFile = "names.sqlite"

// OpenNames opens the name memory in dir, creating it if needed, and drops
// answers older than maxAge.
func OpenNames(dir string, maxAge time.Duration, log *slog.Logger) (*Names, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: creating data directory: %w", err)
	}
	db, err := openDB(filepath.Join(dir, namesFile), "journal_mode(WAL)", "synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("store: opening name memory: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS answers (
		source   TEXT NOT NULL,
		domain   TEXT NOT NULL,
		hosts    TEXT NOT NULL,
		saved_at INTEGER NOT NULL,
		PRIMARY KEY (source, domain)
	) WITHOUT ROWID`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: preparing name memory: %w", err)
	}
	n := &Names{db: db, log: log, maxAge: maxAge}
	if maxAge > 0 {
		_, _ = db.Exec(`DELETE FROM answers WHERE saved_at < ?`, time.Now().Add(-maxAge).Unix())
	}
	return n, nil
}

// Load returns the hostnames source last listed for domain and when, if
// that answer is still kept.
func (n *Names) Load(source, domain string) ([]string, time.Time, bool) {
	var (
		hosts string
		saved int64
	)
	err := n.db.QueryRow(`SELECT hosts, saved_at FROM answers WHERE source = ? AND domain = ?`, source, domain).Scan(&hosts, &saved)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			n.log.Warn("reading the name memory failed", "error", err)
		}
		return nil, time.Time{}, false
	}
	at := time.Unix(saved, 0)
	if n.maxAge > 0 && time.Since(at) > n.maxAge {
		return nil, time.Time{}, false
	}
	if hosts == "" {
		return []string{}, at, true
	}
	return strings.Split(hosts, "\n"), at, true
}

// Save records what source listed for domain just now.
func (n *Names) Save(source, domain string, hosts []string) {
	_, err := n.db.Exec(`INSERT OR REPLACE INTO answers (source, domain, hosts, saved_at) VALUES (?, ?, ?, ?)`,
		source, domain, strings.Join(hosts, "\n"), time.Now().Unix())
	if err != nil {
		n.log.Warn("writing the name memory failed", "error", err)
	}
}

// Close closes the name memory.
func (n *Names) Close() error { return n.db.Close() }

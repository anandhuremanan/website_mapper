package subdomains

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver, for crt.sh's public database

	"websitemapper/internal/resource"
)

// NameLister lists the names on a domain's certificates.
type NameLister interface {
	Names(ctx context.Context, domain string) ([]string, error)
}

// CRTShDB queries crt.sh's public PostgreSQL database directly.
//
// crt.sh's website is a front end to this database, and the database is
// often reachable when the website returns errors or hangs. It is the same
// free, shared service, so it is asked just as sparingly: one query per
// domain, and it may refuse ("no more connections allowed") when busy. A
// query typically takes half a minute or more.
type CRTShDB struct {
	// DSN is the connection string; empty means crt.sh's public database.
	DSN string
	// Pool bounds queries in flight across all scans (nil: unbounded).
	Pool *resource.Pool
	// Timeout bounds one query, including connecting (0: 90 s).
	Timeout time.Duration
}

// The guest account is public and has no password.
const crtshDSN = "host=crt.sh port=5432 user=guest dbname=certwatch sslmode=disable connect_timeout=10"

// crtshQuery lists the names on every certificate that mentions the domain,
// keeping those at or under it. It is crt.sh's own recommended query; the
// reversed LIKE lets the database use its index on reversed names.
//
// The domain is written into the statement: crt.sh's connection pooler
// (PgBouncer in statement mode) does not support prepared statements, which
// is how parameters would be sent. plainDomain guards what is written.
const crtshQuery = `SELECT cai.NAME_VALUE FROM certificate_and_identities cai
WHERE plainto_tsquery('certwatch', '%[1]s') @@ identities(cai.CERTIFICATE)
  AND (lower(cai.NAME_VALUE) = '%[1]s' OR reverse(lower(cai.NAME_VALUE)) LIKE reverse('%%.%[1]s'))
GROUP BY cai.NAME_VALUE`

// plainDomain matches normalized hostnames: nothing that could end the
// quoted string in crtshQuery.
var plainDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func (d *CRTShDB) Names(ctx context.Context, domain string) ([]string, error) {
	domain = strings.ToLower(domain)
	if !plainDomain.MatchString(domain) {
		return nil, fmt.Errorf("crt.sh database: %q is not a plain hostname", domain)
	}
	release, _, err := d.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dsn := d.DSN
	if dsn == "" {
		dsn = crtshDSN
	}
	// A connection per query: the pooler hands out a server per statement,
	// and nothing is gained by keeping an idle connection to a shared service.
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("crt.sh database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// No arguments, so the driver sends this as one simple query.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(crtshQuery, domain))
	if err != nil {
		return nil, describeDB(ctx, err, timeout)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, describeDB(ctx, err, timeout)
		}
		// One value may hold several names, one per line.
		names = append(names, strings.Split(value, "\n")...)
	}
	if err := rows.Err(); err != nil {
		return nil, describeDB(ctx, err, timeout)
	}
	return names, nil
}

func describeDB(ctx context.Context, err error, timeout time.Duration) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("crt.sh database did not answer within %s", timeout)
	case strings.Contains(err.Error(), "no more connections allowed"), strings.Contains(err.Error(), "too many"):
		return errors.New("crt.sh database is busy (no free connections)")
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		msg = msg[i+2:]
	}
	return fmt.Errorf("crt.sh database: %s", msg)
}

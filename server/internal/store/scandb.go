package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver: the server stays a static binary

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
)

// Each scan is one SQLite database file:
//
//	meta   the scan's status record and, once finished, its result header
//	hosts  the result's hosts, in display order (written when it finishes)
//	urls   one row per URL, written while the scan runs
//
// urls is clustered on (host_id, path, origin), so a host's URLs are read
// in path order straight from the table, with no separate index. Text that
// is usually absent is stored as NULL, and sources, hints and types as
// small integers, which keeps a row to roughly the length of its path.
const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE hosts (id INTEGER PRIMARY KEY, rank INTEGER NOT NULL, name TEXT NOT NULL, data BLOB NOT NULL);
CREATE TABLE urls (
	host_id    INTEGER NOT NULL,
	path       TEXT    NOT NULL,
	origin     TEXT    NOT NULL,
	state      INTEGER NOT NULL,
	type       INTEGER NOT NULL,
	kind       INTEGER NOT NULL,
	sources    INTEGER NOT NULL,
	hints      INTEGER NOT NULL,
	status     INTEGER NOT NULL,
	ctype      TEXT,
	title      TEXT,
	redirect   TEXT,
	server     TEXT,
	err        TEXT,
	methods    TEXT,
	refs       TEXT,
	cached_at  INTEGER,
	arch_first INTEGER,
	arch_ctype TEXT,
	PRIMARY KEY (host_id, path, origin)
) WITHOUT ROWID;
`

// schemaVersion changes when the schema does; files of another version are
// discarded at startup rather than migrated (results are short-lived).
const schemaVersion = "1"

const (
	metaSchema = "schema"
	metaScan   = "scan"   // the scan.Scan status record, as JSON
	metaResult = "result" // the scan.Result without hosts, as JSON
)

const urlColumns = `state, type, kind, sources, hints, status, ctype, title, redirect, server, err, methods, refs, cached_at, arch_first, arch_ctype`

// Memory used by SQLite's page cache, per open database. It is allocated
// outside the Go heap, so GOMEMLIMIT does not see it: size it against the
// service's memory limit (see server/README.md).
const (
	writerCacheKB = 8 << 10
	readerCacheKB = 2 << 10
)

// Stored codes. Never renumber: finished results are read back by them.
const (
	stateDiscovered = 0
	stateFailed     = 1
	stateVerified   = 2
)

var (
	typeCodes = []classify.Type{classify.TypeUnknown, classify.TypePage, classify.TypeAPI, classify.TypeAsset}
	kindCodes = []classify.AssetKind{"", classify.AssetJavaScript, classify.AssetStylesheet, classify.AssetImage,
		classify.AssetFont, classify.AssetMedia, classify.AssetDocument, classify.AssetOther}
)

const (
	typePageCode  = 1
	typeAssetCode = 3
)

func code[T comparable](all []T, v T) int {
	for i, x := range all {
		if x == v {
			return i
		}
	}
	return 0
}

func decode[T any](all []T, c int) T {
	if c < 0 || c >= len(all) {
		c = 0
	}
	return all[c]
}

// openDB opens a scan's database with one connection, so settings and
// transactions apply to every statement.
func openDB(path string, pragmas ...string) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)"
	for _, p := range pragmas {
		dsn += "&_pragma=" + p
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// createDB creates a scan's database holding only its status record.
func createDB(path string, sc scan.Scan) error {
	db, err := openDB(path, "synchronous(OFF)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, metaSchema, []byte(schemaVersion)); err != nil {
		return err
	}
	return putJSON(context.Background(), db, metaScan, sc)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putJSON(ctx context.Context, db execer, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, b)
	return err
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// getJSON reads a meta value into v. found is false if the key is absent.
func getJSON(ctx context.Context, db queryer, key string, v any) (found bool, err error) {
	var b []byte
	switch err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&b); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// updateStatus rewrites the status record of a scan whose database is not
// open for writing.
func updateStatus(path string, sc scan.Scan) error {
	db, err := openDB(path, "synchronous(OFF)")
	if err != nil {
		return err
	}
	defer db.Close()
	return putJSON(context.Background(), db, metaScan, sc)
}

// removeDB deletes a scan's database and SQLite's side files.
func removeDB(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writer is a running scan's results.URLStore. The Aggregator serializes
// the URL calls; the store calls the others from its own goroutines, so
// everything takes mu.
type writer struct {
	mu   sync.Mutex
	db   *sql.DB
	conn *sql.Conn // the database's only connection, held for the scan

	insert, get, put, pending *sql.Stmt

	// Writes are grouped into transactions: one per commitEvery rows or
	// commitInterval, whichever comes first.
	inTx    bool
	writes  int
	txStart time.Time

	// cache holds recently used records, so the links repeated on every
	// page of a site (navigation, assets) cost no query. Records are
	// written through, so the cache can be dropped at any time.
	cache  map[urlKey]*results.URLRecord
	closed bool
}

type urlKey struct {
	host         int64
	path, origin string
}

const (
	commitEvery    = 2000
	commitInterval = time.Second
	cacheSize      = 8192
)

var errWriterClosed = errors.New("scan database is closed")

func openWriter(path string) (*writer, error) {
	// WAL lets the result be read while the scan writes it. A crash loses
	// at most the scan in progress, which is discarded anyway, so syncing
	// to disk on every commit would only cost I/O.
	db, err := openDB(path, "journal_mode(WAL)", "synchronous(OFF)", fmt.Sprintf("cache_size(-%d)", writerCacheKB))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	w := &writer{db: db, conn: conn, cache: make(map[urlKey]*results.URLRecord)}
	const cols = `host_id, path, origin, ` + urlColumns
	const marks = `?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`
	for _, st := range []struct {
		stmt **sql.Stmt
		sql  string
	}{
		{&w.insert, `INSERT INTO urls (` + cols + `) VALUES (` + marks + `) ON CONFLICT DO NOTHING`},
		{&w.put, `INSERT OR REPLACE INTO urls (` + cols + `) VALUES (` + marks + `)`},
		{&w.get, `SELECT ` + urlColumns + ` FROM urls WHERE host_id = ? AND path = ? AND origin = ?`},
		{&w.pending, `SELECT host_id, path, origin FROM urls WHERE state = ? AND type = ? AND sources <> ?`},
	} {
		if *st.stmt, err = conn.PrepareContext(ctx, st.sql); err != nil {
			w.Close()
			return nil, err
		}
	}
	return w, nil
}

var _ scan.URLStore = (*writer)(nil)

func (w *writer) Insert(r *results.URLRecord) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false, errWriterClosed
	}
	k := urlKey{r.HostID, r.Path, r.Origin}
	if _, ok := w.cache[k]; ok {
		return false, nil
	}
	if err := w.begin(); err != nil {
		return false, err
	}
	res, err := w.insert.Exec(rowArgs(r)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	w.remember(k, r)
	return true, w.wrote()
}

func (w *writer) Get(hostID int64, path, origin string) (*results.URLRecord, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, false, errWriterClosed
	}
	k := urlKey{hostID, path, origin}
	if r, ok := w.cache[k]; ok {
		return r, true, nil
	}
	r := &results.URLRecord{HostID: hostID, Path: path, Origin: origin}
	switch err := scanRecord(w.get.QueryRow(hostID, path, origin), r); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	w.remember(k, r)
	return r, true, nil
}

func (w *writer) Update(r *results.URLRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	if err := w.begin(); err != nil {
		return err
	}
	if _, err := w.put.Exec(rowArgs(r)...); err != nil {
		return err
	}
	return w.wrote()
}

func (w *writer) PendingPages(fn func(hostID int64, path, origin string)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	rows, err := w.pending.Query(stateDiscovered, typePageCode, int64(results.ArchiveOnly))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k urlKey
		if err := rows.Scan(&k.host, &k.path, &k.origin); err != nil {
			return err
		}
		fn(k.host, k.path, k.origin)
	}
	return rows.Err()
}

func (w *writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	return w.commit()
}

func (w *writer) remember(k urlKey, r *results.URLRecord) {
	if len(w.cache) >= cacheSize {
		clear(w.cache)
	}
	w.cache[k] = r
}

func (w *writer) begin() error {
	if w.inTx {
		return nil
	}
	if _, err := w.conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		return err
	}
	w.inTx, w.writes, w.txStart = true, 0, time.Now()
	return nil
}

func (w *writer) wrote() error {
	w.writes++
	if w.writes < commitEvery && time.Since(w.txStart) < commitInterval {
		return nil
	}
	return w.commit()
}

func (w *writer) commit() error {
	if !w.inTx {
		return nil
	}
	w.inTx = false
	_, err := w.conn.ExecContext(context.Background(), "COMMIT")
	return err
}

// putStatus saves the scan's status record.
func (w *writer) putStatus(sc scan.Scan) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	return putJSON(context.Background(), w.conn, metaScan, sc)
}

// save stores a result's hosts and header beside the URLs already written
// and commits, so that readers see all three together.
func (w *writer) save(r scan.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	return w.saveLocked(r)
}

func (w *writer) saveLocked(r scan.Result) error {
	ctx := context.Background()
	if err := w.begin(); err != nil {
		return err
	}
	if _, err := w.conn.ExecContext(ctx, `DELETE FROM hosts`); err != nil {
		return err
	}
	for rank, h := range r.Hosts {
		h.URLs = nil
		data, err := json.Marshal(h)
		if err != nil {
			return err
		}
		if _, err := w.conn.ExecContext(ctx, `INSERT INTO hosts (id, rank, name, data) VALUES (?, ?, ?, ?)`,
			h.ID, rank, h.Hostname, data); err != nil {
			return err
		}
	}
	header := r
	header.Hosts = nil
	if err := putJSON(ctx, w.conn, metaResult, header); err != nil {
		return err
	}
	return w.commit()
}

// finish saves the final result, then turns the database into a single
// plain file and closes it.
func (w *writer) finish(r scan.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errWriterClosed
	}
	if err := w.saveLocked(r); err != nil {
		return err
	}
	// Fold the write-ahead log into the main file and leave WAL mode, so
	// the finished result is one file that needs no side files to read.
	// This needs the database to itself: if someone is reading the result
	// right now it stays in WAL mode, which works just as well; SQLite
	// folds the log in when the last reader closes.
	var mode string
	_ = w.conn.QueryRowContext(context.Background(), `PRAGMA journal_mode = DELETE`).Scan(&mode)
	return w.closeLocked()
}

// Close commits what was written and closes the database. It is safe to
// call more than once.
func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLocked()
}

func (w *writer) closeLocked() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.cache = nil
	err := w.commit()
	for _, st := range []*sql.Stmt{w.insert, w.get, w.put, w.pending} {
		if st != nil {
			st.Close()
		}
	}
	w.conn.Close()
	if cerr := w.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// rowArgs are a record's column values, in the order of the insert and put
// statements.
func rowArgs(r *results.URLRecord) []any {
	state := stateDiscovered
	switch {
	case r.Responded:
		state = stateVerified
	case r.Err != "":
		state = stateFailed
	}
	var cachedAt, archFirst any
	var archType string
	if r.CachedAt != nil {
		cachedAt = r.CachedAt.UnixNano()
	}
	if r.Archive != nil {
		archFirst, archType = r.Archive.FirstSeen.Unix(), r.Archive.ContentType
	}
	return []any{
		r.HostID, r.Path, r.Origin,
		state, code(typeCodes, r.Type), code(kindCodes, r.AssetKind), int64(r.Sources), int64(r.Hints), r.Status,
		text(r.ContentType), text(r.Title), text(r.Redirect), text(r.Server), text(r.Err),
		text(strings.Join(r.Methods, ",")), text(strings.Join(r.From, "\n")),
		cachedAt, archFirst, text(archType),
	}
}

// text stores an empty string as NULL.
func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanRecord reads the urlColumns of a row into r, after any columns in
// lead (which the caller selected before them).
func scanRecord(row rowScanner, r *results.URLRecord, lead ...any) error {
	var (
		state, typ, kind, sources, hints        int64
		ctype, title, redirect, server, errText sql.NullString
		methods, refs, archType                 sql.NullString
		cachedAt, archFirst                     sql.NullInt64
	)
	dest := append(lead, &state, &typ, &kind, &sources, &hints, &r.Status,
		&ctype, &title, &redirect, &server, &errText, &methods, &refs, &cachedAt, &archFirst, &archType)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	r.Responded = state == stateVerified
	r.Type, r.AssetKind = decode(typeCodes, int(typ)), decode(kindCodes, int(kind))
	r.Sources, r.Hints = results.SourceSet(sources), results.HintSet(hints)
	r.ContentType, r.Title, r.Redirect, r.Server, r.Err = ctype.String, title.String, redirect.String, server.String, errText.String
	r.Methods, r.From = nil, nil
	if methods.String != "" {
		r.Methods = strings.Split(methods.String, ",")
	}
	if refs.String != "" {
		r.From = strings.Split(refs.String, "\n")
	}
	r.CachedAt, r.Archive = nil, nil
	if cachedAt.Valid {
		t := time.Unix(0, cachedAt.Int64).UTC()
		r.CachedAt = &t
	}
	if archFirst.Valid {
		r.Archive = &discovery.ArchiveInfo{FirstSeen: time.Unix(archFirst.Int64, 0).UTC(), ContentType: archType.String}
	}
	return nil
}

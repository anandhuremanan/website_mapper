package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"websitemapper/internal/results"
	"websitemapper/internal/scan"
)

// reader reads one finished result, a page at a time. Every query is a
// range read of the urls table in its stored order (host, path), so a page
// costs the same whether the result has a thousand URLs or a million.
type reader struct {
	db      *sql.DB
	summary scan.Result
	// names maps host IDs to hostnames; loaded on first use.
	names map[int64]string
}

var _ scan.ResultReader = (*reader)(nil)

// nodeURLLimit bounds the URLs listed for one tree node. A node has several
// only when URLs differ by query string or scheme.
const nodeURLLimit = 50

func openReader(ctx context.Context, path string) (*reader, error) {
	db, err := openDB(path, "query_only(1)", fmt.Sprintf("cache_size(-%d)", readerCacheKB))
	if err != nil {
		return nil, err
	}
	r := &reader{db: db}
	found, err := getJSON(ctx, db, metaResult, &r.summary)
	if err == nil && !found {
		err = scan.ErrNotFound
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	r.summary.Hosts = []results.Host{}
	return r, nil
}

func (r *reader) Close() error { return r.db.Close() }

func (r *reader) Summary() scan.Result { return r.summary }

// A cursor is where a listing continues, opaque to clients.
func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return fmt.Errorf("%w: cursor", scan.ErrBadQuery)
	}
	return nil
}

// contains returns a LIKE pattern matching text that contains s.
func contains(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

func (r *reader) Hosts(ctx context.Context, q scan.HostQuery) (scan.HostPage, error) {
	page := scan.HostPage{Hosts: []results.Host{}}
	after := -1
	if q.After != "" {
		if err := decodeCursor(q.After, &after); err != nil {
			return page, err
		}
	}
	query := `SELECT id, rank, data FROM hosts WHERE rank > ?`
	args := []any{after}
	if q.Search != "" {
		query += ` AND name LIKE ? ESCAPE '\'`
		args = append(args, contains(q.Search))
	}
	query += ` ORDER BY rank`
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit+1) // one more, to know whether a page follows
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	rank := after
	for rows.Next() {
		if q.Limit > 0 && len(page.Hosts) == q.Limit {
			page.Next = encodeCursor(rank)
			break
		}
		var (
			h    results.Host
			data []byte
		)
		if err := rows.Scan(&h.ID, &rank, &data); err != nil {
			return page, err
		}
		if err := json.Unmarshal(data, &h); err != nil {
			return page, err
		}
		h.URLs = []results.URL{}
		page.Hosts = append(page.Hosts, h)
	}
	return page, rows.Err()
}

// hostNames loads the hostname of every host ID.
func (r *reader) hostNames(ctx context.Context) (map[int64]string, error) {
	if r.names != nil {
		return r.names, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, name FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[int64]string{}
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	r.names = names
	return names, rows.Err()
}

func (r *reader) hostID(ctx context.Context, hostname string) (int64, bool, error) {
	var id int64
	switch err := r.db.QueryRowContext(ctx, `SELECT id FROM hosts WHERE name = ?`, hostname).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return id, true, nil
}

// urlCursor is the key of the last URL of a page.
type urlCursor struct {
	Host   int64  `json:"h"`
	Path   string `json:"p"`
	Origin string `json:"o"`
}

func (r *reader) URLs(ctx context.Context, q scan.URLQuery, fn func(results.URL) error) (string, error) {
	names, err := r.hostNames(ctx)
	if err != nil {
		return "", err
	}
	query := `SELECT host_id, path, origin, ` + urlColumns + ` FROM urls WHERE 1 = 1`
	var args []any
	if q.Host != "" {
		id, ok, err := r.hostID(ctx, q.Host)
		if err != nil || !ok {
			return "", err
		}
		query += ` AND host_id = ?`
		args = append(args, id)
	}
	if len(q.Types) > 0 {
		marks := make([]string, len(q.Types))
		for i, t := range q.Types {
			marks[i] = "?"
			args = append(args, code(typeCodes, t))
		}
		query += ` AND type IN (` + strings.Join(marks, ", ") + `)`
	}
	if q.Search != "" {
		like := contains(q.Search)
		if q.Host != "" {
			query += ` AND (path LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`
			args = append(args, like, like)
		} else {
			query += ` AND (path LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\' OR host_id IN (SELECT id FROM hosts WHERE name LIKE ? ESCAPE '\'))`
			args = append(args, like, like, like)
		}
	}
	if q.After != "" {
		var c urlCursor
		if err := decodeCursor(q.After, &c); err != nil {
			return "", err
		}
		query += ` AND (host_id, path, origin) > (?, ?, ?)`
		args = append(args, c.Host, c.Path, c.Origin)
	}
	query += ` ORDER BY host_id, path, origin`
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit+1)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var last urlCursor
	n := 0
	for rows.Next() {
		if q.Limit > 0 && n == q.Limit {
			return encodeCursor(last), nil
		}
		var rec results.URLRecord
		if err := scanRecord(rows, &rec, &rec.HostID, &rec.Path, &rec.Origin); err != nil {
			return "", err
		}
		if err := fn(rec.View(names[rec.HostID])); err != nil {
			return "", err
		}
		last = urlCursor{rec.HostID, rec.Path, rec.Origin}
		n++
	}
	return "", rows.Err()
}

// treeCursor is where the scan for the next child continues.
type treeCursor struct {
	Bound  string `json:"b"`
	Strict bool   `json:"s"`
}

// In stored order, the paths at or below a node C are not one range,
// because other names sort in between ("/a", "/a-b", "/a/x", "/a0",
// "/a?q"). They are three: C itself, [C+"/", C+"0") for deeper paths and
// [C+"?", C+"@") for C with a query ("0" follows "/" and "@" follows "?").
const (
	afterSlash = "0"
	afterQuery = "@"
)

// tree runs the queries of one Tree call: all are restricted to one host
// and, unless assets are included, to non-asset URLs.
type tree struct {
	ctx    context.Context
	db     *sql.DB
	host   int64
	filter string
}

func (t *tree) count(where string, args ...any) (int, error) {
	var n int
	err := t.db.QueryRowContext(t.ctx, `SELECT count(*) FROM urls WHERE host_id = ? AND `+where+t.filter,
		append([]any{t.host}, args...)...).Scan(&n)
	return n, err
}

func (t *tree) exact(p string) (int, error) { return t.count(`path = ?`, p) }
func (t *tree) below(p string) (int, error) {
	return t.count(`path >= ? AND path < ?`, p+"/", p+afterSlash)
}

// next returns the first path after bound and before upper.
func (t *tree) next(bound string, strict bool, upper string) (string, bool, error) {
	op := ">="
	if strict {
		op = ">"
	}
	var p string
	err := t.db.QueryRowContext(t.ctx, `SELECT path FROM urls WHERE host_id = ? AND path `+op+` ? AND path < ?`+t.filter+
		` ORDER BY path LIMIT 1`, t.host, bound, upper).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return p, err == nil, err
}

// urls returns the URLs that end exactly at node p: p itself, then p with a
// query. They are read as two ranges; one query with OR would make SQLite
// scan every path of the host.
func (t *tree) urls(hostname, p string) ([]results.URL, error) {
	out := []results.URL{}
	for _, where := range []struct {
		cond string
		args []any
	}{
		{`path = ?`, []any{p}},
		{`path >= ? AND path < ?`, []any{p + "?", p + afterQuery}},
	} {
		args := append(append([]any{t.host}, where.args...), nodeURLLimit-len(out))
		rows, err := t.db.QueryContext(t.ctx, `SELECT path, origin, `+urlColumns+` FROM urls WHERE host_id = ? AND `+where.cond+
			t.filter+` ORDER BY path, origin LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			rec := results.URLRecord{HostID: t.host}
			if err := scanRecord(rows, &rec, &rec.Path, &rec.Origin); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, rec.View(hostname))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Tree lists the children of a path by skipping through the host's paths:
// find the next path, take its segment below the parent as a child, count
// the child's ranges, then continue after them. Finding children costs one
// lookup each, however many URLs lie below them; only their totals read
// the ranges.
func (r *reader) Tree(ctx context.Context, q scan.TreeQuery) (scan.Tree, error) {
	parent := strings.TrimSuffix(q.Path, "/") // "" is the root
	if parent != "" && !strings.HasPrefix(parent, "/") {
		return scan.Tree{}, fmt.Errorf("%w: path must start with /", scan.ErrBadQuery)
	}
	id, ok, err := r.hostID(ctx, q.Host)
	if err != nil {
		return scan.Tree{}, err
	}
	if !ok {
		return scan.Tree{}, scan.ErrNotFound
	}
	t := &tree{ctx: ctx, db: r.db, host: id}
	if !q.Assets {
		t.filter = fmt.Sprintf(` AND type <> %d`, typeAssetCode)
	}
	out := scan.Tree{Path: parent, URLs: []results.URL{}, Children: []scan.TreeNode{}}
	base, upper := parent+"/", parent+afterSlash
	if parent == "" {
		out.Path = "/"
	}

	cur := treeCursor{Bound: base}
	if q.After != "" {
		if err := decodeCursor(q.After, &cur); err != nil {
			return out, err
		}
	} else {
		// First page: the parent's own URLs and its total.
		if parent == "" {
			if out.Total, err = t.count(`1 = 1`); err != nil {
				return out, err
			}
			out.URLs, err = t.urls(q.Host, "/")
		} else {
			if out.Total, err = t.total(parent); err != nil {
				return out, err
			}
			out.URLs, err = t.urls(q.Host, parent)
		}
		if err != nil {
			return out, err
		}
	}

	for {
		before := cur
		path, ok, err := t.next(cur.Bound, cur.Strict, upper)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		rest := path[len(base):]
		switch {
		case rest == "": // the root's own "/"
			cur = treeCursor{Bound: path, Strict: true}
			continue
		case rest[0] == '?': // the root with a query
			cur = treeCursor{Bound: base + afterQuery}
			continue
		}
		name, kind := rest, byte(0)
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			name, kind = rest[:i], rest[i]
		}
		child := base + name

		// A child is listed at its first path. Its later ranges are reached
		// again after other names; skip them.
		listed := false
		switch kind {
		case 0:
			cur = treeCursor{Bound: child, Strict: true}
		case '/':
			cur = treeCursor{Bound: child + afterSlash}
			n, err := t.exact(child)
			if err != nil {
				return out, err
			}
			listed = n > 0
		case '?':
			cur = treeCursor{Bound: child + afterQuery}
			n, err := t.exact(child)
			if err == nil && n == 0 {
				n, err = t.below(child)
			}
			if err != nil {
				return out, err
			}
			listed = n > 0
		}
		if listed {
			continue
		}
		if q.Limit > 0 && len(out.Children) == q.Limit {
			// The page is full and another child follows: continue from
			// just before it.
			out.Next = encodeCursor(before)
			return out, nil
		}

		node := scan.TreeNode{Name: name, Path: child}
		below, err := t.below(child)
		if err != nil {
			return out, err
		}
		own, err := t.own(child)
		if err != nil {
			return out, err
		}
		node.Total, node.HasChildren = own+below, below > 0
		if node.URLs, err = t.urls(q.Host, child); err != nil {
			return out, err
		}
		out.Children = append(out.Children, node)
	}
}

// own counts the URLs that end exactly at node p.
func (t *tree) own(p string) (int, error) {
	exact, err := t.exact(p)
	if err != nil {
		return 0, err
	}
	queries, err := t.count(`path >= ? AND path < ?`, p+"?", p+afterQuery)
	return exact + queries, err
}

// total counts the URLs at or below node p.
func (t *tree) total(p string) (int, error) {
	own, err := t.own(p)
	if err != nil {
		return 0, err
	}
	below, err := t.below(p)
	return own + below, err
}

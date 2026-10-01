// Package api exposes the scan service over a small REST API.
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
)

// Scans is the subset of the scan service the API needs.
type Scans interface {
	Create(ctx context.Context, req scan.CreateRequest) (scan.Scan, error)
	Get(ctx context.Context, id string) (scan.Scan, error)
	OpenResult(ctx context.Context, id string) (scan.ResultReader, error)
	Cancel(ctx context.Context, id, subscription string) (scan.Scan, error)
	Stats() scan.Stats
}

// maxRequestBody bounds JSON request bodies.
const maxRequestBody = 4 << 10

type handler struct {
	scans  Scans
	log    *slog.Logger
	access Access
	starts *startLimiter
}

// NewHandler returns the API's http.Handler.
func NewHandler(scans Scans, log *slog.Logger, access Access) http.Handler {
	h := &handler{scans: scans, log: log, access: access, starts: newStartLimiter(access)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("POST /api/scans", h.createScan)
	mux.HandleFunc("GET /api/scans/{id}", h.getScan)
	mux.HandleFunc("GET /api/scans/{id}/summary", h.getSummary)
	mux.HandleFunc("GET /api/scans/{id}/hosts", h.getHosts)
	mux.HandleFunc("GET /api/scans/{id}/urls", h.getURLs)
	mux.HandleFunc("GET /api/scans/{id}/tree", h.getTree)
	mux.HandleFunc("GET /api/scans/{id}/export", h.exportURLs)
	mux.HandleFunc("GET /api/scans/{id}/results", h.getResults)
	mux.HandleFunc("POST /api/scans/{id}/cancel", h.cancelScan)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return h.middleware(mux)
}

// health reports that the server is up and, to callers allowed to use the
// API, how it is doing. On a closed API anyone may still ask whether it is
// up (uptime checks, the deploy script), but learns nothing more.
func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if !h.access.allowed(r) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "scheduler": h.scans.Stats(), "process": readProcessStats()})
}

// processStats shows how much of the machine the server is using, for
// operating it next to other services.
type processStats struct {
	// HeapMB is live heap memory; SysMB is everything obtained from the OS.
	HeapMB     float64 `json:"heapMB"`
	SysMB      float64 `json:"sysMB"`
	Goroutines int     `json:"goroutines"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	// MemoryLimitMB is GOMEMLIMIT, if set.
	MemoryLimitMB int64  `json:"memoryLimitMB,omitempty"`
	NumGC         uint32 `json:"numGC"`
}

func readProcessStats() processStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st := processStats{
		HeapMB: float64(m.HeapAlloc) / (1 << 20), SysMB: float64(m.Sys) / (1 << 20),
		Goroutines: runtime.NumGoroutine(), GOMAXPROCS: runtime.GOMAXPROCS(0), NumGC: m.NumGC,
	}
	if limit := debug.SetMemoryLimit(-1); limit != math.MaxInt64 {
		st.MemoryLimitMB = limit >> 20
	}
	return st
}

type createRequest struct {
	Target string `json:"target"`
	// Mode is "passive", "light" or "full"; empty means the default.
	Mode string `json:"mode"`
	// Fresh asks for a new scan even if a recent one could be returned.
	Fresh bool `json:"fresh"`
}

func (h *handler) createScan(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `request body must be JSON like {"target": "example.com"}`)
		return
	}

	// The start limit applies only if this request starts a new scan.
	visitor := h.access.visitor(r)
	sc, err := h.scans.Create(r.Context(), scan.CreateRequest{
		Target: req.Target, Mode: scan.Mode(req.Mode), Fresh: req.Fresh,
		BeforeStart: func() error {
			if wait := h.starts.take(visitor); wait > 0 {
				return &startLimitError{wait}
			}
			return nil
		},
	})
	var limited *startLimitError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"You have started %d scans in the last %s, which is the limit. Try again in %s.",
			h.access.StartLimit, plainDuration(h.access.StartWindow), plainDuration(limited.wait)))
		return
	case errors.Is(err, discovery.ErrInvalidTarget), errors.Is(err, scan.ErrInvalidMode):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, scan.ErrQueueFull), errors.Is(err, scan.ErrShuttingDown), errors.Is(err, scan.ErrStorageFull):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		h.log.Error("creating scan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create scan")
		return
	}
	w.Header().Set("Location", "/api/scans/"+sc.ID)
	if sc.Reused {
		// A recent finished scan: nothing was started.
		writeJSON(w, http.StatusOK, sc)
		return
	}
	writeJSON(w, http.StatusAccepted, sc)
}

func (h *handler) getScan(w http.ResponseWriter, r *http.Request) {
	sc, err := h.scans.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, scan.ErrNotFound) {
		writeError(w, http.StatusNotFound, "scan not found")
		return
	}
	if err != nil {
		h.log.Error("loading scan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load scan")
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

type cancelRequest struct {
	SubscriptionID string `json:"subscriptionId"`
}

// cancelScan releases the requester's subscription to a queued or running
// scan; the scan itself stops when no subscription remains. A queued scan
// is cancelled at once; a running scan stops within moments and keeps its
// partial results. The body is optional: {"subscriptionId": "..."}.
func (h *handler) cancelScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req cancelRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, `request body must be empty or JSON like {"subscriptionId": "..."}`)
			return
		}
	}
	sc, err := h.scans.Cancel(r.Context(), id, req.SubscriptionID)
	switch {
	case errors.Is(err, scan.ErrNotFound):
		writeError(w, http.StatusNotFound, "scan not found")
	case errors.Is(err, scan.ErrShared), errors.Is(err, scan.ErrNotSubscribed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, scan.ErrFinished):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "status": string(sc.Status)})
	case err != nil:
		h.log.Error("cancelling scan failed", "scan_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not cancel scan")
	default:
		writeJSON(w, http.StatusAccepted, sc)
	}
}

// Page sizes for the result listings.
const (
	defaultPageSize = 100
	maxPageSize     = 500
)

// streamTimeout is how long a response that streams a whole result (the
// full document, an export) may take; the server's usual write timeout is
// too short for a large result on a slow connection.
const streamTimeout = 10 * time.Minute

// withResult opens a scan's result for fn, answering 404 or 409 itself
// when there is none to read.
func (h *handler) withResult(w http.ResponseWriter, r *http.Request, fn func(rd scan.ResultReader)) {
	id := r.PathValue("id")
	rd, err := h.scans.OpenResult(r.Context(), id)
	if err == nil {
		defer rd.Close()
		fn(rd)
		return
	}
	if !errors.Is(err, scan.ErrNotFound) {
		h.log.Error("loading result failed", "scan_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not load results")
		return
	}
	// Distinguish "no such scan" from "not finished yet".
	sc, err := h.scans.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "scan not found")
		return
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":  "scan has not finished yet",
		"status": string(sc.Status),
	})
}

// fail answers a failed result query: 400 for a bad cursor or path, 404
// for a host the result does not have, 500 otherwise.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, scan.ErrBadQuery):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, scan.ErrNotFound):
		writeError(w, http.StatusNotFound, "host not found in this result")
	default:
		h.log.Error("reading result failed", "scan_id", r.PathValue("id"), "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read results")
	}
}

// pageSize reads the limit parameter, bounded to maxPageSize.
func pageSize(r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultPageSize, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return min(n, maxPageSize), true
}

// urlTypes reads the type parameter: a comma-separated list of URL types.
func urlTypes(r *http.Request) ([]classify.Type, bool) {
	v := r.URL.Query().Get("type")
	if v == "" {
		return nil, true
	}
	var out []classify.Type
	for _, s := range strings.Split(v, ",") {
		switch t := classify.Type(s); t {
		case classify.TypePage, classify.TypeAPI, classify.TypeAsset, classify.TypeUnknown:
			out = append(out, t)
		default:
			return nil, false
		}
	}
	return out, true
}

// getSummary returns a result without its hosts and URLs.
func (h *handler) getSummary(w http.ResponseWriter, r *http.Request) {
	h.withResult(w, r, func(rd scan.ResultReader) {
		writeJSON(w, http.StatusOK, summaryOf(rd.Summary()))
	})
}

// summary is a scan.Result without the "hosts" field. (A nil field with the
// same JSON name hides the embedded one.)
type summary struct {
	scan.Result
	Hosts *struct{} `json:"hosts,omitempty"`
}

func summaryOf(res scan.Result) summary { return summary{Result: res} }

// hostOnly is a results.Host without the "urls" field.
type hostOnly struct {
	results.Host
	URLs *struct{} `json:"urls,omitempty"`
}

func (h *handler) getHosts(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageSize(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be a positive number")
		return
	}
	q := r.URL.Query()
	h.withResult(w, r, func(rd scan.ResultReader) {
		page, err := rd.Hosts(r.Context(), scan.HostQuery{Search: q.Get("q"), After: q.Get("after"), Limit: limit})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		hosts := make([]hostOnly, len(page.Hosts))
		for i, host := range page.Hosts {
			hosts[i] = hostOnly{Host: host}
		}
		writeJSON(w, http.StatusOK, struct {
			Hosts []hostOnly `json:"hosts"`
			Next  string     `json:"next,omitempty"`
		}{hosts, page.Next})
	})
}

func (h *handler) getURLs(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageSize(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be a positive number")
		return
	}
	types, ok := urlTypes(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "type must list page, api, asset or unknown")
		return
	}
	q := r.URL.Query()
	h.withResult(w, r, func(rd scan.ResultReader) {
		urls := []results.URL{}
		next, err := rd.URLs(r.Context(), scan.URLQuery{
			Host: q.Get("host"), Types: types, Search: q.Get("q"), After: q.Get("after"), Limit: limit,
		}, func(u results.URL) error {
			urls = append(urls, u)
			return nil
		})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			URLs []results.URL `json:"urls"`
			Next string        `json:"next,omitempty"`
		}{urls, next})
	})
}

func (h *handler) getTree(w http.ResponseWriter, r *http.Request) {
	limit, ok := pageSize(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be a positive number")
		return
	}
	q := r.URL.Query()
	if q.Get("host") == "" {
		writeError(w, http.StatusBadRequest, "host is required")
		return
	}
	h.withResult(w, r, func(rd scan.ResultReader) {
		tree, err := rd.Tree(r.Context(), scan.TreeQuery{
			Host: q.Get("host"), Path: q.Get("path"), Assets: q.Get("assets") == "true", After: q.Get("after"), Limit: limit,
		})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, tree)
	})
}

// stream prepares a response that sends a whole result.
func stream(w http.ResponseWriter, contentType string) {
	// Not supported by every ResponseWriter (tests); the default applies then.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamTimeout))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
}

// getResults sends a whole result as one JSON document. Clients that show
// results should read them in pages instead (summary, hosts, urls, tree).
func (h *handler) getResults(w http.ResponseWriter, r *http.Request) {
	h.withResult(w, r, func(rd scan.ResultReader) {
		stream(w, "application/json")
		w.WriteHeader(http.StatusOK)
		// The status line is sent, so a failure can only be logged; the
		// client sees a truncated document.
		if err := writeResult(r.Context(), w, rd); err != nil {
			h.log.Warn("sending result failed", "scan_id", r.PathValue("id"), "error", err)
		}
	})
}

// writeResult streams a result as one JSON document: the encoding of a
// scan.Result whose hosts carry their URLs. URLs are read from the store
// and written one at a time, so the response is never held in memory.
func writeResult(ctx context.Context, w io.Writer, rd scan.ResultReader) error {
	hosts, err := rd.Hosts(ctx, scan.HostQuery{})
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 32<<10)

	// Encode everything but the hosts, then reopen the object for them.
	head, err := openObject(summaryOf(rd.Summary()))
	if err != nil {
		return err
	}
	bw.Write(head)
	bw.WriteString(`,"hosts":[`)
	for i, host := range hosts.Hosts {
		if i > 0 {
			bw.WriteByte(',')
		}
		head, err := openObject(hostOnly{Host: host})
		if err != nil {
			return err
		}
		bw.Write(head)
		bw.WriteString(`,"urls":[`)
		first := true
		_, err = rd.URLs(ctx, scan.URLQuery{Host: host.Hostname}, func(u results.URL) error {
			b, err := json.Marshal(u)
			if err != nil {
				return err
			}
			if !first {
				bw.WriteByte(',')
			}
			first = false
			_, err = bw.Write(b)
			return err
		})
		if err != nil {
			return err
		}
		bw.WriteString(`]}`)
	}
	bw.WriteString("]}\n")
	return bw.Flush()
}

// openObject encodes v, a struct, without its closing brace, so that more
// fields can follow.
func openObject(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte("}"))
	if len(b) < 2 { // "{" alone: a following ",field" would be invalid
		return nil, errors.New("cannot extend an empty JSON object")
	}
	return b, nil
}

// exportURLs sends every URL of a result as CSV, one row per URL.
func (h *handler) exportURLs(w http.ResponseWriter, r *http.Request) {
	h.withResult(w, r, func(rd scan.ResultReader) {
		stream(w, "text/csv; charset=utf-8")
		name := rd.Summary().Domain.Canonical
		if name == "" {
			name = "scan"
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`-urls.csv"`)
		w.WriteHeader(http.StatusOK)

		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"url", "host", "type", "asset_kind", "state", "status", "content_type", "title", "sources", "first_archived"})
		_, err := rd.URLs(r.Context(), scan.URLQuery{}, func(u results.URL) error {
			status, archived := "", ""
			if u.Status != 0 {
				status = strconv.Itoa(u.Status)
			}
			if u.Archived != nil {
				archived = u.Archived.FirstSeen.Format(time.DateOnly)
			}
			sources := make([]string, len(u.Sources))
			for i, s := range u.Sources {
				sources[i] = string(s)
			}
			return cw.Write([]string{u.URL, u.Hostname, string(u.Type), string(u.AssetKind), string(u.State), status,
				u.ContentType, plainCell(u.Title), strings.Join(sources, " "), archived})
		})
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
		if err != nil {
			h.log.Warn("sending export failed", "scan_id", r.PathValue("id"), "error", err)
		}
	})
}

// plainCell keeps a spreadsheet from running text taken from a web page
// (a title) as a formula.
func plainCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				h.log.Error("handler panicked", "path", r.URL.Path, "panic", v)
				writeError(rec, http.StatusInternalServerError, "internal error")
			}
			level := slog.LevelInfo
			if (r.Method == http.MethodGet && rec.status < 400) || rec.status == http.StatusUnauthorized {
				// Status polling would otherwise flood the log, and so
				// would strangers knocking on a closed API.
				level = slog.LevelDebug
			}
			h.log.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start).Round(time.Microsecond))
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// A closed API answers only the web client, except for the
		// bare "is it up" check.
		if !h.access.allowed(r) && r.URL.Path != "/api/health" {
			writeError(rec, http.StatusUnauthorized, "this API is only available through the Web Scanner site")
			return
		}
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			gz := newGzipWriter(rec)
			defer gz.Close()
			next.ServeHTTP(gz, r)
			return
		}
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// plainDuration writes a duration the way a person would say it.
func plainDuration(d time.Duration) string {
	switch {
	case d >= 90*time.Second:
		return fmt.Sprintf("%d minutes", int(d.Round(time.Minute).Minutes()))
	case d > 45*time.Second:
		return "a minute"
	}
	return fmt.Sprintf("%d seconds", max(int(d.Round(time.Second).Seconds()), 1))
}

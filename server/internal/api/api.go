// Package api exposes the scan service over a small REST API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/scan"
)

// Scans is the subset of the scan service the API needs.
type Scans interface {
	Create(ctx context.Context, req scan.CreateRequest) (scan.Scan, error)
	Get(ctx context.Context, id string) (scan.Scan, error)
	Result(ctx context.Context, id string) (scan.Result, error)
}

// maxRequestBody bounds JSON request bodies.
const maxRequestBody = 4 << 10

// BotInfo describes the crawler to site owners. It is served at
// GET /api/bot and rendered by the client's /bot page.
type BotInfo struct {
	Name        string `json:"name"`
	UserAgent   string `json:"userAgent"`
	RobotsToken string `json:"robotsToken"`
	InfoURL     string `json:"infoUrl"`
	Contact     string `json:"contact,omitempty"`
	// RespectsRobotsTxt is true when crawling follows robots.txt rules.
	RespectsRobotsTxt bool      `json:"respectsRobotsTxt"`
	Limits            BotLimits `json:"limits"`
}

// BotLimits are the request limits that apply to every scan.
type BotLimits struct {
	RequestsPerSecondPerHost float64 `json:"requestsPerSecondPerHost"`
	RequestsPerSecondPerScan float64 `json:"requestsPerSecondPerScan"`
	MaxRequestsPerScan       int     `json:"maxRequestsPerScan"`
	MaxRequestsPerHost       int     `json:"maxRequestsPerHost"`
	MaxHostsCrawled          int     `json:"maxHostsCrawled"`
	RequestTimeoutSeconds    float64 `json:"requestTimeoutSeconds"`
}

type handler struct {
	scans Scans
	bot   BotInfo
	log   *slog.Logger
}

// NewHandler returns the API's http.Handler.
func NewHandler(scans Scans, bot BotInfo, log *slog.Logger) http.Handler {
	h := &handler{scans: scans, bot: bot, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("GET /api/bot", h.botInfo)
	mux.HandleFunc("POST /api/scans", h.createScan)
	mux.HandleFunc("GET /api/scans/{id}", h.getScan)
	mux.HandleFunc("GET /api/scans/{id}/results", h.getResults)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return h.middleware(mux)
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) botInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.bot)
}

type createRequest struct {
	Target string `json:"target"`
	// AuthorizationConfirmed must be true: the caller confirms they own the
	// domain or have permission to scan it. The UI's consent checkbox sets
	// it, and API clients must send it explicitly.
	AuthorizationConfirmed bool `json:"authorizationConfirmed"`
}

func (h *handler) createScan(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `request body must be JSON like {"target": "example.com", "authorizationConfirmed": true}`)
		return
	}

	sc, err := h.scans.Create(r.Context(), scan.CreateRequest{Target: req.Target, AuthorizationConfirmed: req.AuthorizationConfirmed})
	switch {
	case errors.Is(err, scan.ErrAuthorizationRequired):
		h.log.Warn("scan rejected without authorization confirmation", "event", "scan_rejected", "reason", "authorization_not_confirmed")
		writeError(w, http.StatusBadRequest, `authorization not confirmed: set "authorizationConfirmed": true to confirm you own this domain or have permission to scan it`)
		return
	case errors.Is(err, discovery.ErrInvalidTarget):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, scan.ErrQueueFull):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		h.log.Error("creating scan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create scan")
		return
	}
	w.Header().Set("Location", "/api/scans/"+sc.ID)
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

func (h *handler) getResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := h.scans.Result(r.Context(), id)
	if err == nil {
		writeJSON(w, http.StatusOK, res)
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
			if r.Method == http.MethodGet && rec.status < 400 {
				level = slog.LevelDebug // status polling would otherwise flood the log
			}
			h.log.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start).Round(time.Microsecond))
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

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

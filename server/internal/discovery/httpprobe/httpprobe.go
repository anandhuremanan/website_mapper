// Package httpprobe is a discovery engine that checks whether resolved
// hosts serve HTTP(S), and records basic metadata about their root URL.
package httpprobe

import (
	"context"
	"errors"
	"io"
	"strings"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/htmlmeta"
	"websitemapper/internal/resource"
)

// maxRedirects bounds the redirects followed from a host's root URL.
const maxRedirects = 3

// Fetcher performs a single GET without following redirects.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Response, error)
}

// Options bounds probing.
type Options struct {
	// MaxHosts is the maximum number of hosts probed per scan.
	MaxHosts int
	// Concurrency is the number of hosts probed in parallel.
	Concurrency int
	// AllowPrivate probes hosts that resolve only to non-public addresses.
	// Development only; the fetcher enforces its own policy regardless.
	AllowPrivate bool
}

// Engine probes resolved hosts that have not been probed yet.
type Engine struct {
	fetcher Fetcher
	opts    Options
}

// New creates an Engine.
func New(f Fetcher, opts Options) *Engine {
	return &Engine{fetcher: f, opts: opts}
}

func (e *Engine) Name() string { return "http" }

// Discover probes https://host/ and, if that fails, http://host/ for every
// resolved host, up to MaxHosts per scan.
//
// The two schemes are tried one after the other rather than in parallel:
// most hosts answer HTTPS, so racing both would roughly double the requests
// sent (and the global slots used) for the common case to save time only
// on hosts that fail HTTPS.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	hosts := in.State.Hosts()
	discovery.PrioritizeHosts(hosts, in.Target)

	done := 0
	var todo []string
	for _, h := range hosts {
		switch {
		case h.HTTP != nil:
			done++
		case h.DNS == nil || !h.DNS.Resolved:
			// Not resolved (yet); nothing to connect to.
		case h.DNS.NonPublic && !e.opts.AllowPrivate:
			emit(discovery.Finding{Host: h.Name, HTTP: &discovery.HTTPInfo{Skipped: "resolves only to non-public addresses"}})
		default:
			todo = append(todo, h.Name)
		}
	}
	budget := max(e.opts.MaxHosts-done, 0)
	if len(todo) > budget {
		for _, h := range todo[budget:] {
			emit(discovery.Finding{Host: h, HTTP: &discovery.HTTPInfo{Skipped: discovery.SkipHostLimit}})
		}
		todo = todo[:budget]
	}

	discovery.ForEachLimited(todo, e.opts.Concurrency, func(h string) {
		if ctx.Err() != nil {
			return
		}
		if info := e.probe(ctx, in.Target, h); info != nil {
			emit(discovery.Finding{Host: h, HTTP: info})
		}
	})
	return ctx.Err()
}

// probe returns nil if ctx was cancelled mid-probe, so the host stays unprobed.
func (e *Engine) probe(ctx context.Context, t discovery.Target, host string) *discovery.HTTPInfo {
	var lastErr error
	for _, scheme := range []string{"https", "http"} {
		root := scheme + "://" + host + "/"
		resp, err := e.fetcher.Get(ctx, root)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, resource.ErrBudgetExhausted) {
				return &discovery.HTTPInfo{Skipped: discovery.SkipRequestLimit}
			}
			lastErr = err
			continue
		}
		info := &discovery.HTTPInfo{
			Reachable:   true,
			URL:         root,
			Scheme:      scheme,
			Status:      resp.StatusCode,
			Server:      strings.TrimSpace(resp.Header.Get("Server")),
			ContentType: resp.ContentType,
		}
		if resp.IsRedirect() {
			info.Redirect = resp.Location
		}
		final := e.follow(ctx, t, resp)
		info.FinalURL, info.FinalStatus = final.URL, final.StatusCode
		if final != resp {
			info.ContentType = final.ContentType
		}
		if final.StatusCode >= 200 && final.StatusCode < 300 && strings.Contains(final.ContentType, "html") {
			info.Title = htmlmeta.Title(final.Body)
		}
		return info
	}
	return &discovery.HTTPInfo{Reachable: false, Error: describe(lastErr)}
}

// follow follows in-scope redirects from resp and returns the last response.
func (e *Engine) follow(ctx context.Context, t discovery.Target, resp *fetch.Response) *fetch.Response {
	for i := 0; i < maxRedirects && resp.IsRedirect() && t.URLInScope(resp.Location); i++ {
		next, err := e.fetcher.Get(ctx, resp.Location)
		if err != nil {
			break
		}
		resp = next
	}
	return resp
}

func describe(err error) string {
	switch {
	case err == nil:
		return "no response"
	case errors.Is(err, fetch.ErrBlockedAddress):
		return "resolves to a non-public address"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout"):
		return "connection timed out"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection refused"
	case errors.Is(err, io.EOF) || strings.HasSuffix(err.Error(), "EOF"):
		return "connection closed without a response"
	case strings.Contains(err.Error(), "certificate"):
		return "TLS certificate error: " + lastPart(err.Error())
	}
	return lastPart(err.Error())
}

// lastPart trims Go's nested error prefixes ("Get \"...\": dial tcp: ...").
func lastPart(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}

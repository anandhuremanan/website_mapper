// Package config loads server configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete server configuration.
type Config struct {
	Port     string
	LogLevel slog.Level

	// MaxConcurrentScans bounds how many scans run at the same time.
	MaxConcurrentScans int
	// QueueSize bounds how many scans may wait for a free worker.
	QueueSize int
	// MaxStoredScans bounds how many scans the in-memory store keeps.
	MaxStoredScans int

	Bot BotConfig

	Scan ScanConfig
}

// BotConfig identifies the crawler to the sites it visits.
type BotConfig struct {
	// InfoURL is the public page describing the crawler (the client's /bot
	// page). It is included in the default User-Agent.
	InfoURL string
	// InfoURLSet is false when InfoURL is the development default.
	InfoURLSet bool
	// Contact is how site owners can reach the operator (email or URL).
	Contact string
}

// DefaultBotInfoURL is used when BOT_INFO_URL is not set.
const DefaultBotInfoURL = "http://localhost:3000/bot"

// ScanConfig controls how a single scan behaves.
type ScanConfig struct {
	// RequestTimeout is the timeout for a single outbound HTTP request.
	RequestTimeout time.Duration
	// Timeout is the overall time budget for one scan.
	Timeout time.Duration
	// MaxDepth is how many links deep the crawler follows from a host's start page.
	MaxDepth int
	// MaxURLs is the maximum number of requests the crawler makes to one host.
	MaxURLs int
	// Concurrency is the number of parallel requests to one host.
	Concurrency int
	// MaxRecordedURLsPerHost bounds how many URLs are kept per host.
	MaxRecordedURLsPerHost int
	// MaxHosts is the maximum number of hosts crawled per scan.
	MaxHosts int
	// MaxProbeHosts is the maximum number of hosts probed over HTTP per scan.
	MaxProbeHosts int
	// MaxResolveHosts is the maximum number of hosts resolved per scan.
	MaxResolveHosts int
	// HostConcurrency is the number of hosts probed or crawled in parallel.
	HostConcurrency int
	// DNSTimeout bounds resolving one host.
	DNSTimeout time.Duration
	// CTEnabled turns Certificate Transparency subdomain discovery on or off.
	CTEnabled bool
	// CTTimeout bounds the Certificate Transparency query (crt.sh is slow).
	CTTimeout time.Duration
	// RequestsPerSecond is the per-host request rate limit.
	RequestsPerSecond float64
	// MaxBodyBytes caps how much of a response body is read.
	MaxBodyBytes int64
	// UserAgent is sent with every outbound request.
	UserAgent string
	// RespectRobots applies robots.txt rules while crawling.
	RespectRobots bool
	// MaxRequests bounds one scan's outbound HTTP requests across all hosts.
	MaxRequests int
	// ScanRequestsPerSecond bounds one scan's overall request rate across
	// all hosts. RequestsPerSecond still limits each host.
	ScanRequestsPerSecond float64
	// AllowPrivateNetworks permits requests to loopback/private addresses.
	// Only intended for local development and tests.
	AllowPrivateNetworks bool
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration using the given lookup function.
func LoadFrom(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	infoURL := p.str("BOT_INFO_URL", DefaultBotInfoURL)
	cfg := Config{
		Bot: BotConfig{
			InfoURL:    infoURL,
			InfoURLSet: strings.TrimSpace(getenv("BOT_INFO_URL")) != "",
			Contact:    p.str("BOT_CONTACT", ""),
		},
		Port:               p.str("SERVER_PORT", "8080"),
		LogLevel:           p.level("LOG_LEVEL", slog.LevelInfo),
		MaxConcurrentScans: p.int("MAX_CONCURRENT_SCANS", 4, 1),
		QueueSize:          p.int("SCAN_QUEUE_SIZE", 100, 1),
		MaxStoredScans:     p.int("MAX_STORED_SCANS", 500, 1),
		Scan: ScanConfig{
			RequestTimeout:         p.duration("SCAN_REQUEST_TIMEOUT", 10*time.Second),
			Timeout:                p.duration("SCAN_TIMEOUT", 5*time.Minute),
			MaxDepth:               p.int("SCAN_MAX_DEPTH", 2, 0),
			MaxURLs:                p.int("SCAN_MAX_URLS", 100, 1),
			Concurrency:            p.int("SCAN_CONCURRENCY", 4, 1),
			MaxHosts:               p.int("SCAN_MAX_HOSTS", 20, 1),
			MaxRecordedURLsPerHost: p.int("SCAN_MAX_RECORDED_URLS_PER_HOST", 1000, 1),
			MaxProbeHosts:          p.int("SCAN_MAX_PROBE_HOSTS", 100, 1),
			MaxResolveHosts:        p.int("SCAN_MAX_RESOLVE_HOSTS", 500, 1),
			HostConcurrency:        p.int("SCAN_HOST_CONCURRENCY", 4, 1),
			DNSTimeout:             p.duration("SCAN_DNS_TIMEOUT", 5*time.Second),
			CTEnabled:              p.bool("SCAN_CT_ENABLED", true),
			CTTimeout:              p.duration("SCAN_CT_TIMEOUT", 60*time.Second),
			RequestsPerSecond:      p.float("SCAN_REQUESTS_PER_SECOND", 5),
			MaxBodyBytes:           int64(p.int("SCAN_MAX_BODY_BYTES", 2<<20, 1024)),
			UserAgent:              p.str("SCAN_USER_AGENT", "WebsiteMapperBot/0.1 (+"+infoURL+")"),
			RespectRobots:          p.bool("SCAN_RESPECT_ROBOTS", true),
			MaxRequests:            p.int("SCAN_MAX_REQUESTS", 3000, 1),
			ScanRequestsPerSecond:  p.float("SCAN_MAX_REQUESTS_PER_SECOND", 20),
			AllowPrivateNetworks:   p.bool("SCAN_ALLOW_PRIVATE_NETWORKS", false),
		},
	}
	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(p.errs, "; "))
	}
	return cfg, nil
}

type parser struct {
	getenv func(string) string
	errs   []string
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) int(key string, def, min int) int {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		p.errs = append(p.errs, fmt.Sprintf("%s must be an integer >= %d", key, min))
		return def
	}
	return n
}

func (p *parser) float(key string, def float64) float64 {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		p.errs = append(p.errs, fmt.Sprintf("%s must be a positive number", key))
		return def
	}
	return f
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.errs = append(p.errs, fmt.Sprintf("%s must be a positive duration such as 10s", key))
		return def
	}
	return d
}

func (p *parser) bool(key string, def bool) bool {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s must be true or false", key))
		return def
	}
	return b
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v := strings.TrimSpace(p.getenv(key))
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s must be one of debug, info, warn, error", key))
		return def
	}
	return l
}

// RobotsAgent returns the product token robots.txt rules are matched
// against: the User-Agent up to the first "/" or space, e.g.
// "WebsiteMapperBot".
func (c ScanConfig) RobotsAgent() string {
	ua := strings.TrimSpace(c.UserAgent)
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		ua = ua[:i]
	}
	return ua
}

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

	// MaxConcurrentScans bounds how many scans run at the same time; the
	// rest wait in a FIFO queue.
	MaxConcurrentScans int
	// QueueSize bounds how many scans may wait to run.
	QueueSize int
	Store     StoreConfig

	// GlobalHTTPConcurrency bounds HTTP requests in flight across all scans.
	GlobalHTTPConcurrency int
	// GlobalDNSConcurrency bounds DNS lookups in flight across all scans.
	GlobalDNSConcurrency int
	// GlobalDownloadBytesPerSec caps the bytes all scans download together
	// (0: unlimited). It also caps monthly transfer: rate x ~2.6M seconds.
	GlobalDownloadBytesPerSec int64

	Cache CacheConfig

	Scan ScanConfig
}

// StoreConfig controls where scans and their results are kept on disk and
// when finished ones are removed. Queued and running scans are never removed.
type StoreConfig struct {
	// DataDir holds one database file per scan.
	DataDir string
	// MaxScans bounds how many finished scans are kept.
	MaxScans int
	// MaxBytes bounds the disk space finished scans use together.
	MaxBytes int64
	// MaxAge is how long a finished scan is kept.
	MaxAge time.Duration
	// MinFreeBytes is the free disk space below which new scans are refused.
	MinFreeBytes int64
}

// CacheConfig sizes the shared discovery cache.
type CacheConfig struct {
	// MaxBytes is the whole cache's memory budget (estimated); 0 disables
	// caching. It is split between the layers below.
	MaxBytes int64
	// CertTTL, DNSTTL, ProbeTTL and PageTTL are how long each layer's
	// entries are reused.
	CertTTL    time.Duration
	DNSTTL     time.Duration
	ProbeTTL   time.Duration
	PageTTL    time.Duration
	ArchiveTTL time.Duration
}

// ScanConfig controls how a single scan behaves.
type ScanConfig struct {
	// DefaultMode is the scan mode used when a request does not choose one:
	// "passive", "light" or "full".
	DefaultMode string
	// ArchiveEnabled turns web archive route discovery on or off.
	ArchiveEnabled bool
	// ArchiveMaxURLs bounds the URLs listed from the web archive per scan.
	ArchiveMaxURLs int
	// SitemapMaxURLs and SitemapMaxFiles bound what is read from one host's
	// sitemaps.
	SitemapMaxURLs  int
	SitemapMaxFiles int
	// RequestTimeout is the timeout for a single outbound HTTP request.
	RequestTimeout time.Duration
	// Timeout is the overall time budget for one scan.
	Timeout time.Duration
	// MaxRequests bounds one scan's outbound HTTP requests.
	MaxRequests int
	// MaxDownloadBytes bounds the bytes one scan downloads.
	MaxDownloadBytes int64
	// MaxDiscoveredHosts bounds the hostnames one scan records.
	MaxDiscoveredHosts int
	// MaxRecordedURLs is a safety ceiling on the URLs one scan records, so
	// that a single scan cannot fill the disk.
	MaxRecordedURLs int
	// MaxDepth is how many links deep the crawler follows from a host's start page.
	MaxDepth int
	// MaxURLs is the maximum number of requests the crawler makes to one host.
	MaxURLs int
	// Concurrency is the number of parallel requests to one host.
	Concurrency int
	// MaxHosts is the maximum number of hosts crawled per scan.
	MaxHosts int
	// MaxProbeHosts is the maximum number of hosts probed over HTTP per scan.
	MaxProbeHosts int
	// MaxResolveHosts is the maximum number of hosts resolved per scan.
	MaxResolveHosts int
	// HostConcurrency is how many hosts one scan probes or crawls in parallel.
	// Crawls are rate-limited per host, so this sets one scan's crawl ceiling
	// (hosts x per-host rate); the global HTTP pool decides what actually runs.
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
	cfg := Config{
		Port:               p.str("SERVER_PORT", "8080"),
		LogLevel:           p.level("LOG_LEVEL", slog.LevelInfo),
		MaxConcurrentScans: p.int("MAX_CONCURRENT_SCANS", 3, 1),
		QueueSize:          p.int("SCAN_QUEUE_SIZE", 100, 1),
		Store: StoreConfig{
			DataDir:      p.str("DATA_DIR", "data"),
			MaxScans:     p.int("MAX_STORED_SCANS", 500, 1),
			MaxBytes:     int64(p.int("STORE_MAX_MB", 5120, 1)) << 20,
			MaxAge:       p.duration("STORE_MAX_AGE", 7*24*time.Hour),
			MinFreeBytes: int64(p.int("STORE_MIN_FREE_MB", 1024, 0)) << 20,
		},
		GlobalHTTPConcurrency:     p.int("GLOBAL_HTTP_CONCURRENCY", 32, 1),
		GlobalDNSConcurrency:      p.int("GLOBAL_DNS_CONCURRENCY", 16, 1),
		GlobalDownloadBytesPerSec: int64(p.int("GLOBAL_DOWNLOAD_KBPS", 1024, 0)) << 10,
		Cache: CacheConfig{
			MaxBytes: int64(p.int("CACHE_MAX_MB", 64, 0)) << 20,
			CertTTL:  p.duration("CACHE_CERT_TTL", 6*time.Hour),
			DNSTTL:   p.duration("CACHE_DNS_TTL", 5*time.Minute),
			ProbeTTL: p.duration("CACHE_PROBE_TTL", 2*time.Minute),
			PageTTL:  p.duration("CACHE_PAGE_TTL", 15*time.Minute),
			// Archives change slowly and archive.org asks for moderate use.
			ArchiveTTL: p.duration("CACHE_ARCHIVE_TTL", 24*time.Hour),
		},
		Scan: ScanConfig{
			DefaultMode:          p.oneOf("SCAN_DEFAULT_MODE", "light", "passive", "light", "full"),
			ArchiveEnabled:       p.bool("SCAN_ARCHIVE_ENABLED", true),
			ArchiveMaxURLs:       p.int("SCAN_ARCHIVE_MAX_URLS", 5000, 1),
			SitemapMaxURLs:       p.int("SCAN_SITEMAP_MAX_URLS", 2000, 1),
			SitemapMaxFiles:      p.int("SCAN_SITEMAP_MAX_FILES", 5, 1),
			RequestTimeout:       p.duration("SCAN_REQUEST_TIMEOUT", 10*time.Second),
			Timeout:              p.duration("SCAN_TIMEOUT", 30*time.Minute),
			MaxRequests:          p.int("SCAN_MAX_REQUESTS", 50000, 1),
			MaxDownloadBytes:     int64(p.int("SCAN_MAX_DOWNLOAD_MB", 500, 1)) << 20,
			MaxDiscoveredHosts:   p.int("SCAN_MAX_DISCOVERED_HOSTS", 10000, 1),
			MaxRecordedURLs:      p.int("SCAN_MAX_RECORDED_URLS", 1000000, 1),
			MaxDepth:             p.int("SCAN_MAX_DEPTH", 3, 0),
			MaxURLs:              p.int("SCAN_MAX_URLS", 500, 1),
			Concurrency:          p.int("SCAN_CONCURRENCY", 4, 1),
			MaxHosts:             p.int("SCAN_MAX_HOSTS", 500, 1),
			MaxProbeHosts:        p.int("SCAN_MAX_PROBE_HOSTS", 2000, 1),
			MaxResolveHosts:      p.int("SCAN_MAX_RESOLVE_HOSTS", 10000, 1),
			HostConcurrency:      p.int("SCAN_HOST_CONCURRENCY", 8, 1),
			DNSTimeout:           p.duration("SCAN_DNS_TIMEOUT", 5*time.Second),
			CTEnabled:            p.bool("SCAN_CT_ENABLED", true),
			CTTimeout:            p.duration("SCAN_CT_TIMEOUT", 60*time.Second),
			RequestsPerSecond:    p.float("SCAN_REQUESTS_PER_SECOND", 5),
			MaxBodyBytes:         int64(p.int("SCAN_MAX_BODY_BYTES", 2<<20, 1024)),
			UserAgent:            p.str("SCAN_USER_AGENT", "WebsiteMapper/0.1 (+passive public discovery)"),
			AllowPrivateNetworks: p.bool("SCAN_ALLOW_PRIVATE_NETWORKS", false),
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

func (p *parser) oneOf(key, def string, allowed ...string) string {
	v := p.str(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.errs = append(p.errs, fmt.Sprintf("%s must be one of %s", key, strings.Join(allowed, ", ")))
	return def
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

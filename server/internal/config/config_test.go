package config

import (
	"log/slog"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.Scan.RequestTimeout != 10*time.Second {
		t.Errorf("RequestTimeout = %v", cfg.Scan.RequestTimeout)
	}
	if cfg.Scan.AllowPrivateNetworks {
		t.Error("AllowPrivateNetworks should default to false")
	}
	if !cfg.Scan.CTEnabled || cfg.Scan.MaxHosts != 500 || cfg.Scan.MaxURLs != 500 || cfg.Scan.MaxRequests != 50000 {
		t.Errorf("host defaults = %+v", cfg.Scan)
	}
	if cfg.MaxConcurrentScans != 3 || cfg.GlobalHTTPConcurrency != 32 || cfg.GlobalDNSConcurrency != 16 || cfg.Scan.Timeout != 30*time.Minute {
		t.Errorf("resource defaults = %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{
		"SERVER_PORT":               "9000",
		"SCAN_REQUEST_TIMEOUT":      "3s",
		"SCAN_MAX_DEPTH":            "0",
		"SCAN_MAX_URLS":             "20",
		"SCAN_CONCURRENCY":          "2",
		"LOG_LEVEL":                 "debug",
		"SCAN_MAX_HOSTS":            "5",
		"SCAN_CT_ENABLED":           "false",
		"GLOBAL_HTTP_CONCURRENCY":   "8",
		"GLOBAL_DNS_CONCURRENCY":    "2",
		"MAX_CONCURRENT_SCANS":      "1",
		"SCAN_MAX_REQUESTS":         "100",
		"SCAN_MAX_DISCOVERED_HOSTS": "50",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9000" || cfg.Scan.RequestTimeout != 3*time.Second || cfg.Scan.MaxDepth != 0 ||
		cfg.Scan.MaxURLs != 20 || cfg.Scan.Concurrency != 2 || cfg.LogLevel != slog.LevelDebug ||
		cfg.Scan.MaxHosts != 5 || cfg.Scan.CTEnabled || cfg.GlobalHTTPConcurrency != 8 || cfg.GlobalDNSConcurrency != 2 ||
		cfg.MaxConcurrentScans != 1 || cfg.Scan.MaxRequests != 100 || cfg.Scan.MaxDiscoveredHosts != 50 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	_, err := LoadFrom(env(map[string]string{
		"SCAN_MAX_URLS":        "zero",
		"SCAN_REQUEST_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("expected error for invalid values")
	}
}

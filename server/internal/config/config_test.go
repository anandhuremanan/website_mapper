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
	if !cfg.Scan.CTEnabled || cfg.Scan.MaxHosts != 20 || cfg.Scan.MaxURLs != 100 {
		t.Errorf("host defaults = %+v", cfg.Scan)
	}
	if !cfg.Scan.RespectRobots || cfg.Scan.MaxRequests != 3000 || cfg.Scan.ScanRequestsPerSecond != 20 {
		t.Errorf("safeguard defaults = %+v", cfg.Scan)
	}
	if cfg.Scan.UserAgent != "WebsiteMapperBot/0.1 (+http://localhost:3000/bot)" || cfg.Scan.RobotsAgent() != "WebsiteMapperBot" {
		t.Errorf("user agent = %q / %q", cfg.Scan.UserAgent, cfg.Scan.RobotsAgent())
	}
	if cfg.Bot.InfoURLSet {
		t.Error("InfoURLSet should be false by default")
	}
}

func TestLoadBotIdentity(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{
		"BOT_INFO_URL":        "https://mapper.example/bot",
		"BOT_CONTACT":         "abuse@mapper.example",
		"SCAN_RESPECT_ROBOTS": "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scan.UserAgent != "WebsiteMapperBot/0.1 (+https://mapper.example/bot)" || !cfg.Bot.InfoURLSet ||
		cfg.Bot.Contact != "abuse@mapper.example" || cfg.Scan.RespectRobots {
		t.Errorf("cfg = %+v / %+v", cfg.Bot, cfg.Scan)
	}

	cfg, _ = LoadFrom(env(map[string]string{"SCAN_USER_AGENT": "CustomBot/2.0 (+https://x.example)"}))
	if cfg.Scan.RobotsAgent() != "CustomBot" {
		t.Errorf("robots agent = %q", cfg.Scan.RobotsAgent())
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := LoadFrom(env(map[string]string{
		"SERVER_PORT":          "9000",
		"SCAN_REQUEST_TIMEOUT": "3s",
		"SCAN_MAX_DEPTH":       "0",
		"SCAN_MAX_URLS":        "20",
		"SCAN_CONCURRENCY":     "2",
		"LOG_LEVEL":            "debug",
		"SCAN_MAX_HOSTS":       "5",
		"SCAN_CT_ENABLED":      "false",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9000" || cfg.Scan.RequestTimeout != 3*time.Second || cfg.Scan.MaxDepth != 0 ||
		cfg.Scan.MaxURLs != 20 || cfg.Scan.Concurrency != 2 || cfg.LogLevel != slog.LevelDebug ||
		cfg.Scan.MaxHosts != 5 || cfg.Scan.CTEnabled {
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

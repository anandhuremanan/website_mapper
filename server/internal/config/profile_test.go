package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestDeployProfileLoads checks deploy/websitemapper.env parses cleanly.
func TestDeployProfileLoads(t *testing.T) {
	f, err := os.Open("../../deploy/websitemapper.env")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	cfg, err := LoadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentScans != 2 || cfg.GlobalDownloadBytesPerSec != 512<<10 || cfg.Cache.MaxBytes != 32<<20 {
		t.Errorf("profile not applied: %+v", cfg)
	}
}

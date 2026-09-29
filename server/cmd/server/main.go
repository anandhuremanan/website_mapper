// Command server runs the Website Mapper HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"websitemapper/internal/api"
	"websitemapper/internal/config"
	"websitemapper/internal/discovery"
	"websitemapper/internal/discovery/dnsresolve"
	"websitemapper/internal/discovery/htmlcrawl"
	"websitemapper/internal/discovery/httpprobe"
	"websitemapper/internal/discovery/subdomains"
	"websitemapper/internal/fetch"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if !cfg.Bot.InfoURLSet {
		log.Warn("BOT_INFO_URL is not set; the crawler User-Agent points to " + config.DefaultBotInfoURL +
			". Set it to your public /bot page before scanning real sites")
	}
	if cfg.Bot.Contact == "" {
		log.Warn("BOT_CONTACT is not set; site owners will have no way to contact the operator")
	}
	if !cfg.Scan.RespectRobots {
		log.Warn("SCAN_RESPECT_ROBOTS is disabled; robots.txt rules will not be applied")
	}
	if cfg.Scan.AllowPrivateNetworks {
		log.Warn("SCAN_ALLOW_PRIVATE_NETWORKS is enabled; scans may reach private addresses")
	}

	svc := scan.NewService(store.NewMemory(cfg.MaxStoredScans), pipeline(cfg.Scan, log), scan.Options{
		Workers:           cfg.MaxConcurrentScans,
		QueueSize:         cfg.QueueSize,
		ScanTimeout:       cfg.Scan.Timeout,
		MaxURLsPerHost:    cfg.Scan.MaxRecordedURLsPerHost,
		MaxRequests:       cfg.Scan.MaxRequests,
		RequestsPerSecond: cfg.Scan.ScanRequestsPerSecond,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc.Start(ctx)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.NewHandler(svc, botInfo(cfg), log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("server listening", "event", "server_started", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down", "event", "server_stopping")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	svc.Wait() // workers observe ctx cancellation and stop running scans
	return err
}

// pipeline defines the scan's stages, in order:
//
//  1. passive discovery of hostnames (Certificate Transparency)
//  2. DNS resolution of every known host
//  3. HTTP(S) probing of resolved hosts
//  4. crawling of reachable hosts, which may reveal more hosts
//  5. resolving, probing and crawling hosts first found in step 4
//
// Host engines only process hosts they have not seen, so the same engine
// instances are reused in the follow-up stage. Future engines (robots.txt,
// sitemap, JavaScript analysis) slot in as engines or stages here.
func pipeline(cfg config.ScanConfig, log *slog.Logger) []scan.Stage {
	client := fetch.New(fetch.Options{
		Timeout:           cfg.RequestTimeout,
		MaxBodyBytes:      cfg.MaxBodyBytes,
		UserAgent:         cfg.UserAgent,
		RequestsPerSecond: cfg.RequestsPerSecond,
		AllowPrivate:      cfg.AllowPrivateNetworks,
	})

	dns := dnsresolve.New(net.DefaultResolver, dnsresolve.Options{
		MaxHosts:    cfg.MaxResolveHosts,
		Concurrency: 2 * cfg.HostConcurrency,
		Timeout:     cfg.DNSTimeout,
	})
	probe := httpprobe.New(client, httpprobe.Options{
		MaxHosts:     cfg.MaxProbeHosts,
		Concurrency:  cfg.HostConcurrency,
		AllowPrivate: cfg.AllowPrivateNetworks,
	})
	crawler := htmlcrawl.New(client, htmlcrawl.Options{
		MaxHosts:        cfg.MaxHosts,
		MaxPagesPerHost: cfg.MaxURLs,
		MaxDepth:        cfg.MaxDepth,
		Concurrency:     cfg.Concurrency,
		HostConcurrency: cfg.HostConcurrency,
		RespectRobots:   cfg.RespectRobots,
		RobotsAgent:     cfg.RobotsAgent(),
	}, log)

	var stages []scan.Stage
	if cfg.CTEnabled {
		// crt.sh is slow and returns large responses for busy domains, so it
		// gets its own client with a longer timeout and a larger body limit.
		ctClient := fetch.New(fetch.Options{
			Timeout:           cfg.CTTimeout,
			MaxBodyBytes:      64 << 20,
			UserAgent:         cfg.UserAgent,
			RequestsPerSecond: 1,
		})
		// Two independent Certificate Transparency providers: public CT
		// services fail often, and results are merged either way.
		sources := []subdomains.Source{subdomains.NewCRTSh(ctClient), subdomains.NewCertSpotter(ctClient)}
		stages = append(stages, scan.Stage{ID: "subdomains", Label: "Discovering subdomains",
			Engines: []discovery.Engine{subdomains.New(sources, log)}})
	}
	return append(stages,
		scan.Stage{ID: "resolve", Label: "Resolving discovered hosts", Engines: []discovery.Engine{dns}},
		scan.Stage{ID: "probe", Label: "Probing hosts", Engines: []discovery.Engine{probe}},
		scan.Stage{ID: "crawl", Label: "Crawling reachable hosts", Engines: []discovery.Engine{crawler}},
		scan.Stage{ID: "follow-up", Label: "Checking hosts found while crawling", Engines: []discovery.Engine{dns, probe, crawler}},
	)
}

// botInfo describes the crawler for the /bot page.
func botInfo(cfg config.Config) api.BotInfo {
	return api.BotInfo{
		Name:              cfg.Scan.RobotsAgent(),
		UserAgent:         cfg.Scan.UserAgent,
		RobotsToken:       cfg.Scan.RobotsAgent(),
		InfoURL:           cfg.Bot.InfoURL,
		Contact:           cfg.Bot.Contact,
		RespectsRobotsTxt: cfg.Scan.RespectRobots,
		Limits: api.BotLimits{
			RequestsPerSecondPerHost: cfg.Scan.RequestsPerSecond,
			RequestsPerSecondPerScan: cfg.Scan.ScanRequestsPerSecond,
			MaxRequestsPerScan:       cfg.Scan.MaxRequests,
			MaxRequestsPerHost:       cfg.Scan.MaxURLs,
			MaxHostsCrawled:          cfg.Scan.MaxHosts,
			RequestTimeoutSeconds:    cfg.Scan.RequestTimeout.Seconds(),
		},
	}
}

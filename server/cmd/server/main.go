// Command server runs the Web Scanner HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"websitemapper/internal/api"
	"websitemapper/internal/cache"
	"websitemapper/internal/config"
	"websitemapper/internal/discovery"
	"websitemapper/internal/discovery/archive"
	"websitemapper/internal/discovery/dnsresolve"
	"websitemapper/internal/discovery/htmlcrawl"
	"websitemapper/internal/discovery/httpprobe"
	"websitemapper/internal/discovery/sitemap"
	"websitemapper/internal/discovery/subdomains"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
)

// shutdownGrace is how long running scans get to stop and save partial
// results when the server shuts down.
const shutdownGrace = 15 * time.Second

// ctConcurrency bounds Certificate Transparency queries across all scans.
// crt.sh responses can be tens of megabytes, so few may be held at once;
// each scan makes one query per provider.
const ctConcurrency = 4

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
	if cfg.Scan.AllowPrivateNetworks {
		log.Warn("SCAN_ALLOW_PRIVATE_NETWORKS is enabled; scans may reach private addresses")
	}

	// Shared by every scan: these bound the server's total outbound work.
	pools := sharedPools{
		http: resource.NewPool("http", cfg.GlobalHTTPConcurrency),
		dns:  resource.NewPool("dns", cfg.GlobalDNSConcurrency),
		ct:   resource.NewPool("certificate-transparency", ctConcurrency),
		// archive.org asks for moderate use of its index.
		archive: resource.NewPool("archive", 2),
		// Every byte any scan downloads, including certificate-log answers.
		bandwidth: resource.NewBandwidth(cfg.GlobalDownloadBytesPerSec),
	}
	caches := newCaches(cfg.Cache)
	defaultMode, _ := scan.ParseMode(cfg.Scan.DefaultMode, scan.ModeLight)
	// Scans and their results are kept on disk, one file per scan, so the
	// server's memory does not grow with what scans find.
	repo, err := store.Open(store.Options{
		Dir:          cfg.Store.DataDir,
		MaxScans:     cfg.Store.MaxScans,
		MaxBytes:     cfg.Store.MaxBytes,
		MaxAge:       cfg.Store.MaxAge,
		MinFreeBytes: cfg.Store.MinFreeBytes,
		Log:          log,
	})
	if err != nil {
		return err
	}
	defer repo.Close()
	svc := scan.NewService(repo, pipeline(cfg.Scan, pools, caches, log), scan.Options{
		DefaultMode:      defaultMode,
		MaxRunning:       cfg.MaxConcurrentScans,
		QueueSize:        cfg.QueueSize,
		ScanTimeout:      cfg.Scan.Timeout,
		ReuseTTL:         cfg.Scan.ReuseTTL,
		MaxRequests:      cfg.Scan.MaxRequests,
		MaxDownloadBytes: cfg.Scan.MaxDownloadBytes,
		Bandwidth:        pools.bandwidth,
		Limits: results.Limits{
			MaxHosts: cfg.Scan.MaxDiscoveredHosts,
			MaxURLs:  cfg.Scan.MaxRecordedURLs,
		},
		Pools:  []*resource.Pool{pools.http, pools.dns, pools.ct, pools.archive},
		Caches: caches.list(),
	}, log)
	svc.Start(context.Background())
	log.Info("scan scheduler ready", "event", "scheduler_started", "max_concurrent_scans", cfg.MaxConcurrentScans,
		"queue_size", cfg.QueueSize, "global_http", cfg.GlobalHTTPConcurrency, "global_dns", cfg.GlobalDNSConcurrency,
		"scan_timeout", cfg.Scan.Timeout, "download_kbps", cfg.GlobalDownloadBytesPerSec>>10,
		"gomaxprocs", runtime.GOMAXPROCS(0))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.NewHandler(svc, log),
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

	// Stop taking scans, cancel queued ones and interrupt running ones
	// (which save partial results), then stop serving HTTP. Status
	// requests keep working while scans wind down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := svc.Shutdown(shutdownCtx); err != nil {
		log.Warn("scans did not stop within the grace period", "event", "shutdown_timeout", "error", err)
	}
	err = srv.Shutdown(shutdownCtx)
	log.Info("server stopped", "event", "server_stopped")
	return err
}

type sharedPools struct {
	http, dns, ct, archive *resource.Pool
	bandwidth              *resource.Bandwidth
}

// sharedCaches hold reusable discovery data for all scans. They sit inside
// the engines, in front of the network work, so a hit takes no pool slot
// and no request budget; a miss uses the pools like any other request.
type sharedCaches struct {
	cert    *subdomains.Cache
	archive *archive.Cache
	dns     *dnsresolve.Cache
	probe   *httpprobe.Cache
	sitemap *sitemap.Cache
	page    *htmlcrawl.Cache
	ttl     config.CacheConfig
}

// newCaches splits the memory budget between the layers. Pages dominate
// (each holds its outgoing references); archive listings are large but one
// per domain; certificate answers, DNS answers and probe results are small.
// A zero budget disables caching (nil caches always load).
func newCaches(cfg config.CacheConfig) sharedCaches {
	c := sharedCaches{ttl: cfg}
	if cfg.MaxBytes <= 0 {
		return c
	}
	c.page = htmlcrawl.NewCache(cfg.MaxBytes * 50 / 100)
	c.archive = archive.NewCache(cfg.MaxBytes * 20 / 100)
	c.sitemap = sitemap.NewCache(cfg.MaxBytes * 10 / 100)
	c.dns = dnsresolve.NewCache(cfg.MaxBytes * 10 / 100)
	c.probe = httpprobe.NewCache(cfg.MaxBytes * 5 / 100)
	c.cert = subdomains.NewCache(cfg.MaxBytes * 5 / 100)
	return c
}

func (c sharedCaches) list() []interface{ Stats() cache.Stats } {
	if c.page == nil {
		return nil
	}
	return []interface{ Stats() cache.Stats }{c.cert, c.archive, c.dns, c.probe, c.sitemap, c.page}
}

// pipeline defines the scan's stages, in order:
//
//  1. passive discovery of routes and hostnames (web archive), started
//     first and left running in the background
//  2. passive discovery of hostnames (Certificate Transparency)
//  3. DNS resolution of every known host
//  4. HTTP(S) probing of resolved hosts (light, full)
//  5. robots.txt and sitemaps of reachable hosts (light, full)
//  6. crawling of reachable hosts, which may reveal more hosts (full)
//  7. once the archive is done, the same steps for hosts first found by it
//     or in 5 or 6
//
// Host engines only process hosts they have not seen, so the same engine
// instances are reused in the follow-up stage. New engines (for example
// JavaScript analysis) slot in as engines or stages here.
//
// Every engine shares one HTTP client (and so one connection pool) and the
// server-wide resource pools; per-scan concurrency settings only bound how
// much of that shared capacity one scan can ask for at once.
func pipeline(cfg config.ScanConfig, pools sharedPools, caches sharedCaches, log *slog.Logger) []scan.Stage {
	client := fetch.New(fetch.Options{
		Timeout:           cfg.RequestTimeout,
		MaxBodyBytes:      cfg.MaxBodyBytes,
		UserAgent:         cfg.UserAgent,
		RequestsPerSecond: cfg.RequestsPerSecond,
		AllowPrivate:      cfg.AllowPrivateNetworks,
		Pool:              pools.http,
		Bandwidth:         pools.bandwidth,
		// The crawler parses only HTML and the probe only reads HTML titles;
		// other responses are classified from their status and headers.
		ReadBody: fetch.HTMLOnly,
	})

	dns := dnsresolve.New(net.DefaultResolver, dnsresolve.Options{
		MaxHosts:    cfg.MaxResolveHosts,
		Concurrency: 2 * cfg.HostConcurrency,
		Timeout:     cfg.DNSTimeout,
		Pool:        pools.dns,
		Cache:       caches.dns,
		TTL:         caches.ttl.DNSTTL,
		NegativeTTL: time.Minute,
	})
	probe := httpprobe.New(client, httpprobe.Options{
		MaxHosts:     cfg.MaxProbeHosts,
		Concurrency:  cfg.HostConcurrency,
		AllowPrivate: cfg.AllowPrivateNetworks,
		Cache:        caches.probe,
		TTL:          caches.ttl.ProbeTTL,
	})
	crawler := htmlcrawl.New(client, htmlcrawl.Options{
		MaxHosts:        cfg.MaxHosts,
		MaxPagesPerHost: cfg.MaxURLs,
		MaxDepth:        cfg.MaxDepth,
		Concurrency:     cfg.Concurrency,
		HostConcurrency: cfg.HostConcurrency,
		Cache:           caches.page,
		CacheTTL:        caches.ttl.PageTTL,
	}, log)

	sitemaps := sitemap.New(client.WithBodies(fetch.TextAndXML, 4<<20), sitemap.Options{
		MaxHosts:        cfg.MaxProbeHosts,
		MaxFilesPerHost: cfg.SitemapMaxFiles,
		MaxURLsPerHost:  cfg.SitemapMaxURLs,
		Concurrency:     cfg.HostConcurrency,
		Cache:           caches.sitemap,
		TTL:             caches.ttl.PageTTL,
	})

	// Which stages run depends on the scan mode:
	//   passive: certificate logs, web archive, DNS (never contacts the site)
	//   light:   + one probe and robots.txt/sitemaps per live host
	//   full:    + crawling pages
	all := []scan.Mode(nil)
	lightAndFull := []scan.Mode{scan.ModeLight, scan.ModeFull}
	var stages []scan.Stage
	if cfg.ArchiveEnabled {
		archiveClient := fetch.New(fetch.Options{
			Timeout:      cfg.CTTimeout,
			MaxBodyBytes: 16 << 20,
			UserAgent:    cfg.UserAgent,
			Pool:         pools.archive,
			Bandwidth:    pools.bandwidth,
		})
		// The archive is slow and nothing has to wait for its routes, so it
		// runs in the background while hosts are discovered and checked. The
		// follow-up stages join it, to check any hosts only it found.
		stages = append(stages, scan.Stage{ID: "archive", Label: "Searching web archives", Modes: all, Background: true,
			Engines: []discovery.Engine{archive.New(archiveClient, archive.Options{
				MaxURLs: cfg.ArchiveMaxURLs, Cache: caches.archive, TTL: caches.ttl.ArchiveTTL, FailureTTL: 5 * time.Minute,
			})}})
	}
	if cfg.CTEnabled {
		// crt.sh is slow and returns large responses for busy domains, so it
		// gets its own client with a longer timeout and a larger body limit.
		// SCAN_CT_TIMEOUT also bounds each provider in total, retries
		// included, so one slow provider cannot hold up the scan.
		ctClient := fetch.New(fetch.Options{
			Timeout:           cfg.CTTimeout,
			MaxBodyBytes:      64 << 20,
			UserAgent:         cfg.UserAgent,
			RequestsPerSecond: 1,
			Pool:              pools.ct,
			Bandwidth:         pools.bandwidth,
		})
		// Two independent Certificate Transparency providers: public CT
		// services fail often, and results are merged either way.
		sources := []subdomains.Source{subdomains.NewCRTSh(ctClient), subdomains.NewCertSpotter(ctClient)}
		stages = append(stages, scan.Stage{ID: "subdomains", Label: "Discovering subdomains", Modes: all,
			Engines: []discovery.Engine{subdomains.New(sources, cfg.CTTimeout, log).WithCache(subdomains.CacheOptions{
				Cache: caches.cert, TTL: caches.ttl.CertTTL, RateLimitTTL: 15 * time.Minute, FailureTTL: 5 * time.Minute,
			})}})
	}
	return append(stages,
		scan.Stage{ID: "resolve", Label: "Resolving discovered hosts", Modes: all, Engines: []discovery.Engine{dns}},
		scan.Stage{ID: "probe", Label: "Checking which hosts are live", Modes: lightAndFull, Engines: []discovery.Engine{probe}},
		scan.Stage{ID: "sitemaps", Label: "Reading robots.txt and sitemaps", Modes: lightAndFull, Engines: []discovery.Engine{sitemaps}},
		scan.Stage{ID: "crawl", Label: "Crawling reachable hosts", Modes: []scan.Mode{scan.ModeFull}, Engines: []discovery.Engine{crawler}},
		// One follow-up round per mode, for hosts first seen by the archive,
		// in sitemaps or while crawling.
		scan.Stage{ID: "follow-up", Label: "Checking hosts found in the web archive", Modes: []scan.Mode{scan.ModePassive},
			Join: true, Engines: []discovery.Engine{dns}},
		scan.Stage{ID: "follow-up", Label: "Checking hosts found later", Modes: []scan.Mode{scan.ModeLight},
			Join: true, Engines: []discovery.Engine{dns, probe, sitemaps}},
		scan.Stage{ID: "follow-up", Label: "Checking hosts found later", Modes: []scan.Mode{scan.ModeFull},
			Join: true, Engines: []discovery.Engine{dns, probe, sitemaps, crawler}},
	)
}

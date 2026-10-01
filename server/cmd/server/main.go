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

// lookupConcurrency bounds the quick subdomain lookups across all scans.
// Their answers are small and arrive in about a second.
const lookupConcurrency = 8

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
		// The quick subdomain lookups have a pool of their own: sharing the
		// certificate-log pool would leave them waiting behind providers
		// that take a minute, and they would time out in the queue.
		lookups: resource.NewPool("subdomain-lookup", lookupConcurrency),
		// archive.org asks for moderate use of its index; the pace of
		// requests is set by the archive client's rate limit.
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
		Pools:  []*resource.Pool{pools.http, pools.dns, pools.ct, pools.lookups, pools.archive},
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
	http, dns, ct, lookups, archive *resource.Pool
	bandwidth                       *resource.Bandwidth
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
		ConnectTimeout:    cfg.ConnectTimeout,
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

	// One scan may use the whole DNS pool; the pool shares it fairly when
	// several scans resolve at once.
	dnsOptions := dnsresolve.Options{
		MaxHosts:    cfg.MaxResolveHosts,
		Concurrency: pools.dns.Stats().Capacity,
		Timeout:     cfg.DNSTimeout,
		Pool:        pools.dns,
		Cache:       caches.dns,
		TTL:         caches.ttl.DNSTTL,
		NegativeTTL: time.Minute,
	}
	// Names are resolved by asking DNS servers directly, one question per
	// host, which is several times less work than the system resolver does
	// for the same answer. The system resolver takes over if the servers
	// cannot be reached, and is what connections to hosts always use.
	dns := dnsresolve.New(net.DefaultResolver, dnsOptions)
	if len(cfg.DNSServers) > 0 {
		dns = dnsresolve.NewWith(&dnsresolve.Client{
			Servers: cfg.DNSServers, Timeout: 2 * time.Second, Fallback: net.DefaultResolver, Log: log,
		}, dnsOptions)
	}
	probe := httpprobe.New(client, httpprobe.Options{
		MaxHosts: cfg.MaxProbeHosts,
		// Every probe goes to a different host, so they need no spacing;
		// the shared pool bounds them.
		Concurrency:  max(cfg.HostConcurrency, pools.http.Stats().Capacity),
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
		// The listing is read a page at a time. This client is shared by
		// every scan, so its rate limit is the server's whole pace towards
		// the archive: one request every 3 s, 20 a minute, which is below
		// the Internet Archive's limit however many scans are running.
		archiveClient := fetch.New(fetch.Options{
			Timeout:           cfg.CTTimeout,
			MaxBodyBytes:      32 << 20,
			UserAgent:         cfg.UserAgent,
			RequestsPerSecond: 1.0 / 3,
			Pool:              pools.archive,
			Bandwidth:         pools.bandwidth,
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
		stages = append(stages, subdomainStages(cfg, pools, caches, log)...)
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

// fastSourceTimeout bounds each quick subdomain provider in total.
const fastSourceTimeout = 10 * time.Second

// slowSourceWait is how long a scan that has nothing else left to do waits
// for the slow subdomain providers. They keep being asked after that, up to
// SCAN_CT_TIMEOUT, and their answers are cached for later scans.
const slowSourceWait = 5 * time.Second

// subdomainStages builds the stages that ask public sources for subdomains.
//
// The providers differ a great deal in speed. Lookup services answer in
// about a second, while the certificate logs' own search often takes a
// minute or fails. So the quick ones form the stage the scan waits for, and
// the slow ones run in the background. The follow-up stage joins them and
// checks the hosts only they found, but waits at most slowSourceWait: a
// provider that is still silent then is left to finish for the cache, and
// the scan says it was not included. With no quick provider enabled, the
// scan waits for the slow ones as it used to.
func subdomainStages(cfg config.ScanConfig, pools sharedPools, caches sharedCaches, log *slog.Logger) []scan.Stage {
	// crt.sh is slow and returns large responses for busy domains, so the
	// certificate-log providers get a client with a long timeout and a
	// large body limit. SCAN_CT_TIMEOUT also bounds each provider in total,
	// retries included.
	slowClient := fetch.New(fetch.Options{
		Timeout:           cfg.CTTimeout,
		MaxBodyBytes:      64 << 20,
		UserAgent:         cfg.UserAgent,
		RequestsPerSecond: 1,
		Pool:              pools.ct,
		Bandwidth:         pools.bandwidth,
	})
	quickClient := fetch.New(fetch.Options{
		Timeout:           fastSourceTimeout,
		MaxBodyBytes:      16 << 20,
		UserAgent:         cfg.UserAgent,
		RequestsPerSecond: 1,
		Pool:              pools.lookups,
		Bandwidth:         pools.bandwidth,
	})
	var quick, slow []subdomains.Source
	for _, name := range cfg.SubdomainSources {
		switch name {
		case "crtsh":
			slow = append(slow, subdomains.NewCRTSh(slowClient))
		case "certspotter":
			slow = append(slow, subdomains.NewCertSpotter(slowClient))
		case "anubis":
			quick = append(quick, subdomains.NewAnubis(quickClient))
		case "thc":
			quick = append(quick, subdomains.NewTHC(quickClient))
		case "shodan":
			quick = append(quick, subdomains.NewShodanCT(quickClient))
		}
	}
	engine := func(sources []subdomains.Source, timeout time.Duration) []discovery.Engine {
		return []discovery.Engine{subdomains.New(sources, timeout, log).WithCache(subdomains.CacheOptions{
			Cache: caches.cert, TTL: caches.ttl.CertTTL, RateLimitTTL: 15 * time.Minute, FailureTTL: 5 * time.Minute,
		})}
	}
	switch {
	case len(quick) > 0 && len(slow) > 0:
		return []scan.Stage{
			{ID: "certificates", Label: "Searching certificate logs", Background: true, JoinWait: slowSourceWait,
				Engines: engine(slow, cfg.CTTimeout)},
			{ID: "subdomains", Label: "Discovering subdomains", Engines: engine(quick, fastSourceTimeout)},
		}
	case len(quick) > 0:
		return []scan.Stage{{ID: "subdomains", Label: "Discovering subdomains", Engines: engine(quick, fastSourceTimeout)}}
	case len(slow) > 0:
		return []scan.Stage{{ID: "subdomains", Label: "Discovering subdomains", Engines: engine(slow, cfg.CTTimeout)}}
	}
	return nil
}

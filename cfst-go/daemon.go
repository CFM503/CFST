package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ConfigToProbeConfig converts daemon CLI Config to ProbeConfig.
// It starts strictly with DefaultProbeConfig() (ProfileCFST) as the base configuration.
// Only profile-independent scheduling parameters are copied from cfg.
// Profile remains strictly ProfileCFST unless cfg.Profile is explicitly "GOWAY-WSS" or "CUSTOM".
// It NEVER automatically switches to GOWAY-WSS due to cfg.WSSHost being non-empty.
func ConfigToProbeConfig(cfg Config) ProbeConfig {
	probeCfg := DefaultProbeConfig()

	if cfg.ActiveInterval > 0 {
		probeCfg.ActiveInterval = time.Duration(cfg.ActiveInterval) * time.Second
	}
	if cfg.StandbyInterval > 0 {
		probeCfg.StandbyInterval = time.Duration(cfg.StandbyInterval) * time.Second
	}
	if cfg.CandidateInterval > 0 {
		probeCfg.CandidateInterval = time.Duration(cfg.CandidateInterval) * time.Second
	}
	if cfg.FailedInterval > 0 {
		probeCfg.FailedInterval = time.Duration(cfg.FailedInterval) * time.Second
	}
	if cfg.QuickDuration > 0 {
		probeCfg.QuickDuration = cfg.QuickDuration
	}
	if cfg.Duration > 0 {
		probeCfg.FullDuration = cfg.Duration
	}
	if cfg.DiscoveryInterval > 0 {
		probeCfg.DiscoveryInterval = time.Duration(cfg.DiscoveryInterval) * time.Second
	}
	if cfg.DiscoveryScanCount > 0 {
		probeCfg.DiscoveryScanCount = cfg.DiscoveryScanCount
	}
	probeCfg.DiscoveryEnabled = cfg.DiscoveryEnabled

	// Profile is strictly explicit!
	switch strings.ToUpper(strings.TrimSpace(cfg.Profile)) {
	case "GOWAY-WSS", "GOWAY_WSS", "WSS":
		probeCfg.Profile = NewProfileGOWAYWSS(cfg.WSSHost, "/pyway", cfg.SNI, cfg.Port)
	case "CUSTOM":
		probeCfg.Profile = NewProfileCustom(cfg.URL, cfg.SNI, cfg.Port)
	default:
		// Keep DefaultProbeConfig()'s ProfileCFST!
		// WSSHost does NOT cause automatic switch to GOWAY-WSS.
	}

	NormalizeProbeConfig(&probeCfg)
	return probeCfg
}

// RunDaemon initializes and runs CFST as a continuous Route Quality Probe daemon.
func RunDaemon(cfg Config) {
	probeCfg := ConfigToProbeConfig(cfg)
	GlobalProbeScheduler.UpdateConfig(probeCfg)
	GlobalScoreEngine.SetMode(ScoreMode(cfg.ScoreMode))

	fmt.Println("============================================================")
	fmt.Println("   CFST Route Quality Probe v2.1.8 (Continuous Daemon Mode)")
	fmt.Printf("   Listening on: http://%s\n", cfg.APIAddr)
	fmt.Printf("   Score Mode:   %s\n", cfg.ScoreMode)
	fmt.Printf("   Intervals:    Active: %ds | Standby: %ds | Candidate: %ds | Failed: %ds\n",
		int(probeCfg.ActiveInterval.Seconds()),
		int(probeCfg.StandbyInterval.Seconds()),
		int(probeCfg.CandidateInterval.Seconds()),
		int(probeCfg.FailedInterval.Seconds()))
	fmt.Printf("   Discovery:    Enabled: %v | Interval: %ds | ScanCount: %d\n",
		probeCfg.DiscoveryEnabled,
		int(probeCfg.DiscoveryInterval.Seconds()),
		probeCfg.DiscoveryScanCount)
	fmt.Printf("   Profile:      %s\n", probeCfg.Profile.Type)
	fmt.Printf("   Protocol:     %s\n", strings.ToUpper(probeCfg.Profile.Protocol))
	if probeCfg.Profile.Type == ProfileGOWAYWSS {
		fmt.Printf("   Host:         %s\n", probeCfg.Profile.Host)
		fmt.Printf("   SNI:          %s\n", probeCfg.Profile.SNI)
		fmt.Printf("   Path:         %s\n", probeCfg.Profile.Path)
	} else {
		fmt.Printf("   Test URL:     %s\n", probeCfg.Profile.TestURL)
		fmt.Printf("   SNI:          %s\n", probeCfg.Profile.SNI)
		fmt.Printf("   Host:         %s\n", probeCfg.Profile.Host)
	}
	fmt.Println("============================================================")

	// 1. Try loading cached snapshot from disk
	if cfg.StateFile != "" {
		if err := GlobalRouteStore.LoadSnapshot(cfg.StateFile); err == nil {
			fmt.Printf("📂 Loaded %d cached routes from %s\n", len(GlobalRouteStore.GetAll()), cfg.StateFile)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. If no routes exist in store, run Initial Scan to seed candidate pool
	if len(GlobalRouteStore.GetAll()) == 0 {
		fmt.Println("🔍 No prior routes found. Running Initial Scan to seed candidate pool...")
		seedCandidates(ctx, cfg)
	}

	// 3. Start background periodic snapshot saver
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if cfg.StateFile != "" {
					_ = GlobalRouteStore.SaveSnapshot(cfg.StateFile)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// 4. Start background probe scheduler and discovery worker
	GlobalProbeScheduler.Start(ctx)
	fmt.Println("⚡ Background Probe Scheduler started.")

	if probeCfg.DiscoveryEnabled {
		GlobalDiscoveryManager.Start(ctx)
		fmt.Printf("🔭 Continuous Discovery Engine started (interval: %ds, scan count: %d).\n",
			int(probeCfg.DiscoveryInterval.Seconds()), probeCfg.DiscoveryScanCount)
	}

	// 5. Mount Web UI on DefaultServeMux if not already mounted
	RegisterWebRoutes(cfg)

	server := &http.Server{
		Addr:    cfg.APIAddr,
		Handler: http.DefaultServeMux,
	}

	// Graceful shutdown listener
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n🛑 Shutting down Route Quality Probe daemon...")
		cancel()
		GlobalDiscoveryManager.Stop()
		GlobalProbeScheduler.Stop()
		if cfg.StateFile != "" {
			_ = GlobalRouteStore.SaveSnapshot(cfg.StateFile)
			fmt.Printf("💾 Persisted route state to %s\n", cfg.StateFile)
		}
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer sCancel()
		_ = server.Shutdown(shutdownCtx)
		os.Exit(0)
	}()

	fmt.Printf("🚀 Route Quality Probe API running at http://%s/api/routes\n", cfg.APIAddr)
	ln, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
		fmt.Printf("[!] Failed to bind API address %s: %v\n", cfg.APIAddr, err)
		return
	}
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Printf("Server error: %v\n", err)
	}
}

// seedCandidates runs an initial scan to discover candidates and populate the store,
// strictly driven by the active ProbeProfile in GlobalProbeScheduler.
func seedCandidates(ctx context.Context, cfg Config) {
	probeCfg := GlobalProbeScheduler.GetConfig()
	ips := GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	if len(ips) == 0 {
		return
	}

	fmt.Printf("  Scanning %d IPs with Profile: %s (concurrency: %d)...\n", len(ips), probeCfg.Profile.Type, cfg.ScanConcurrent)
	validNodes := ScanRoutesWithProfile(ctx, ips, probeCfg.Port, cfg.ScanConcurrent, probeCfg.Profile, func(done, total, valid int) {
		if done%50 == 0 || done == total {
			fmt.Printf("\r  Seed %s Scan: %d/%d (Valid: %d)", probeCfg.Profile.Type, done, total, valid)
		}
	})
	fmt.Println()

	if len(validNodes) == 0 {
		fmt.Println("  [!] Initial scan found no valid IPs.")
		return
	}

	sort.Slice(validNodes, func(i, j int) bool {
		return validNodes[i].TCPLatency < validNodes[j].TCPLatency
	})

	candidates := validNodes
	if len(candidates) > cfg.TopN {
		candidates = candidates[:cfg.TopN]
	}

	// Quick filter
	quickCfg := cfg
	quickCfg.DLConc = 4
	quickCfg.Profile = string(probeCfg.Profile.Type)
	quickCfg.URL = probeCfg.Profile.TestURL
	quickCfg.SNI = probeCfg.Profile.SNI
	candidates = runQuickFilter(ctx, candidates, quickCfg, cfg.DownloadNum, nil)

	// Test and insert into store
	testCfg := cfg
	testCfg.Profile = string(probeCfg.Profile.Type)
	testCfg.URL = probeCfg.Profile.TestURL
	testCfg.SNI = probeCfg.Profile.SNI
	results := runParallelDownloadTest(ctx, candidates, testCfg, nil, nil, nil, nil)
	for i, res := range results {
		tier := TierCandidate
		if i == 0 {
			tier = TierActive
		} else if i <= 2 {
			tier = TierStandby
		}
		rm := FromNodeResult(res, tier)
		GlobalRouteStore.UpsertRoute(*rm)
	}

	fmt.Printf("  ✅ Seeded %d candidate routes into probe store.\n", len(results))
}

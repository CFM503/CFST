package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

// RunDaemon initializes and runs CFST as a continuous Route Quality Probe daemon.
func RunDaemon(cfg Config) {
	fmt.Println("============================================================")
	fmt.Println("   CFST Route Quality Probe v2.0.0 (Continuous Daemon Mode)")
	fmt.Printf("   Listening on: http://%s\n", cfg.APIAddr)
	fmt.Printf("   Score Mode:   %s\n", cfg.ScoreMode)
	fmt.Printf("   Intervals:    Active: %ds | Standby: %ds | Candidate: %ds | Failed: %ds\n",
		cfg.ActiveInterval, cfg.StandbyInterval, cfg.CandidateInterval, cfg.FailedInterval)
	fmt.Println("============================================================")

	GlobalScoreEngine.SetMode(ScoreMode(cfg.ScoreMode))

	probeCfg := ProbeConfig{
		ActiveInterval:    time.Duration(cfg.ActiveInterval) * time.Second,
		StandbyInterval:   time.Duration(cfg.StandbyInterval) * time.Second,
		CandidateInterval: time.Duration(cfg.CandidateInterval) * time.Second,
		FailedInterval:    time.Duration(cfg.FailedInterval) * time.Second,
		MaxConcurrent:     4,
		MaxSpeedTests:     1,
		QuickDuration:     cfg.QuickDuration,
		FullDuration:      cfg.Duration,
		SpeedTestCycle:    6,
		WSSHost:           cfg.WSSHost,
		SNI:               cfg.SNI,
		URL:               cfg.URL,
		Port:              cfg.Port,
	}
	GlobalProbeScheduler.UpdateConfig(probeCfg)

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

	// 4. Start background probe scheduler
	GlobalProbeScheduler.Start(ctx)
	fmt.Println("⚡ Background Probe Scheduler started.")

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

// seedCandidates runs an initial scan to discover candidates and populate the store.
func seedCandidates(ctx context.Context, cfg Config) {
	ips := GenerateIPs(cfg.MaxScan, cfg.Unique, cfg.IPFile)
	if len(ips) == 0 {
		return
	}

	fmt.Printf("  Scanning %d IPs (concurrency: %d)...\n", len(ips), cfg.ScanConcurrent)
	validNodes := ScanPing(ctx, ips, cfg.Port, cfg.ScanConcurrent, cfg.WSSHost, func(done, total, valid int) {
		if done%50 == 0 || done == total {
			fmt.Printf("\r  Seed Ping Scan: %d/%d (Valid: %d)", done, total, valid)
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
	candidates = runQuickFilter(ctx, candidates, quickCfg, cfg.DownloadNum, nil)

	// Test and insert into store
	results := runParallelDownloadTest(ctx, candidates, cfg, nil, nil, nil, nil)
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

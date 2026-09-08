package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRouteStoreSaveSnapshotConcurrentRace exercises the real production paths
// RecordProbeResult (writer) concurrently with SaveSnapshot (reader/marshaler).
// On the old code SaveSnapshot marshaled live *RouteRecord pointers after RUnlock,
// racing AddSample/EWMA updates. Fixed code deep-copies under RLock.
func TestRouteStoreSaveSnapshotConcurrentRace(t *testing.T) {
	store := NewRouteStore()
	base := time.Now()
	ips := []string{"10.10.0.1", "10.10.0.2", "10.10.0.3", "10.10.0.4", "10.10.0.5"}
	for _, ip := range ips {
		store.UpsertRoute(RouteMetrics{
			IP:         ip,
			Port:       443,
			Tier:       TierCandidate,
			Health:     HealthHealthy,
			LastTested: base,
		})
	}

	tmpDir := t.TempDir()
	var wg sync.WaitGroup

	// Concurrent writers: real RecordProbeResult path (EWMA + samples + scores + tiers).
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ip := ips[(worker+i)%len(ips)]
				m := RouteMetrics{
					IP:               ip,
					Port:             443,
					Tier:             TierCandidate,
					SingleSpeed:      20.0 + float64(i%10),
					P10Speed:         18.0 + float64(i%8),
					MinSpeed:         15.0,
					RTT:              20.0 + float64(i%5),
					PacketLoss:       0.0,
					Jitter:           2.0,
					Stability:        90.0,
					HandshakeSuccess: true,
					LastTested:       base.Add(time.Duration(worker*50+i) * time.Second),
				}
				store.RecordProbeResult(m, true)
			}
		}(w)
	}

	// Concurrent snapshotters: real SaveSnapshot path to distinct files.
	for s := 0; s < 2; s++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			path := filepath.Join(tmpDir, filepath.FromSlash("concurrent_"+string(rune('a'+slot))+".json"))
			for i := 0; i < 20; i++ {
				if err := store.SaveSnapshot(path); err != nil {
					t.Errorf("SaveSnapshot failed: %v", err)
					return
				}
			}
		}(s)
	}

	// Concurrent readers: real GetAll/GetBest/history/tier-evaluation paths.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = store.GetAll()
				_ = store.GetBest(3, "")
				_ = store.GetHistoryWindows(ips[0])
				store.EvaluateTierTransitions(time.Now())
			}
		}()
	}

	wg.Wait()

	// Final snapshot must be valid JSON loadable into a fresh store.
	finalPath := filepath.Join(tmpDir, "final.json")
	if err := store.SaveSnapshot(finalPath); err != nil {
		t.Fatalf("final SaveSnapshot failed: %v", err)
	}
	fresh := NewRouteStore()
	if err := fresh.LoadSnapshot(finalPath); err != nil {
		t.Fatalf("LoadSnapshot of concurrent snapshot failed: %v", err)
	}
	if len(fresh.GetAll()) != len(ips) {
		t.Fatalf("expected %d routes after reload, got %d", len(ips), len(fresh.GetAll()))
	}
}

// TestProbeSchedulerCtxCancelRestartNoClobber verifies the ctx-cancel -> restart
// lifecycle: a stale goroutine exiting via old ctx.Done() must not clear the
// running flag of a newer generation started with a fresh context.
func TestProbeSchedulerCtxCancelRestartNoClobber(t *testing.T) {
	store := NewRouteStore()
	scheduler := NewProbeScheduler(store, DefaultProbeConfig())

	// Generation 1 with cancellable context.
	ctx1, cancel1 := context.WithCancel(context.Background())
	started1 := make(chan struct{})
	exited1 := make(chan struct{})
	scheduler.onStartGoroutine = func() { close(started1) }
	scheduler.onExitGoroutine = func() { close(exited1) }
	scheduler.Start(ctx1)
	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for scheduler generation 1 to start")
	}
	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler generation 1 to be running")
	}

	// Cancel old context and wait for generation 1 to exit via ctx.Done().
	cancel1()
	select {
	case <-exited1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for scheduler generation 1 to exit after ctx cancel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for scheduler.running.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to report stopped after ctx cancel")
	}

	// Generation 2 with a fresh context.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	started2 := make(chan struct{})
	exited2 := make(chan struct{})
	scheduler.onStartGoroutine = func() { close(started2) }
	scheduler.onExitGoroutine = func() { close(exited2) }
	scheduler.Start(ctx2)
	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for scheduler generation 2 to start")
	}
	if !scheduler.running.Load() {
		t.Fatalf("expected scheduler generation 2 to be running")
	}

	// Allow any stale generation-1 cleanup to (incorrectly) fire; it must not
	// clobber generation 2's running flag.
	time.Sleep(200 * time.Millisecond)
	if !scheduler.running.Load() {
		t.Fatalf("stale ctx-cancel clobbered restarted scheduler: running=false after restart")
	}

	scheduler.Stop()
	select {
	case <-exited2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for scheduler generation 2 to exit")
	}
	if scheduler.running.Load() {
		t.Fatalf("expected scheduler to be stopped after final Stop()")
	}
	scheduler.mu.RLock()
	staleCh := scheduler.stopCh
	scheduler.mu.RUnlock()
	if staleCh != nil {
		t.Fatalf("expected stopCh to be nil after clean shutdown")
	}
}

// TestDiscoveryManagerCtxCancelRestartNoClobber mirrors the scheduler test for
// DiscoveryManager: old ctx-cancel must not clear a restarted generation.
func TestDiscoveryManagerCtxCancelRestartNoClobber(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.DiscoveryEnabled = false // keep test offline: no immediate scan, no ticker fire
	sched := NewProbeScheduler(store, cfg)
	dm := NewDiscoveryManager(store, sched)
	dm.scanFunc = func(ctx context.Context, ips []string, port int, concurrent int, profile ProbeProfile, progress func(done, total, valid int)) ([]NodeResult, int, int) {
		return nil, 0, 0
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	started1 := make(chan struct{})
	exited1 := make(chan struct{})
	dm.onStartGoroutine = func() { close(started1) }
	dm.onExitGoroutine = func() { close(exited1) }
	dm.Start(ctx1)
	select {
	case <-started1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery generation 1 to start")
	}
	if !dm.running.Load() {
		t.Fatalf("expected discovery generation 1 to be running")
	}

	cancel1()
	select {
	case <-exited1:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery generation 1 to exit after ctx cancel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for dm.running.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dm.running.Load() {
		t.Fatalf("expected discovery to report stopped after ctx cancel")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	started2 := make(chan struct{})
	exited2 := make(chan struct{})
	dm.onStartGoroutine = func() { close(started2) }
	dm.onExitGoroutine = func() { close(exited2) }
	dm.Start(ctx2)
	select {
	case <-started2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery generation 2 to start")
	}
	if !dm.running.Load() {
		t.Fatalf("expected discovery generation 2 to be running")
	}

	time.Sleep(200 * time.Millisecond)
	if !dm.running.Load() {
		t.Fatalf("stale ctx-cancel clobbered restarted discovery manager")
	}

	dm.Stop()
	select {
	case <-exited2:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for discovery generation 2 to exit")
	}
	if dm.running.Load() {
		t.Fatalf("expected discovery to be stopped after final Stop()")
	}
}

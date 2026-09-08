# Changelog

## v2.1.8 (2026-09-08)

### Release: Stabilization & Cross-Platform CI Reliability Release
- **恢复并确认 v2.1.7 稳定基线**:
  - 严格保持 v2.1.7 经过充分验证的线路质量探测、多周期分层探针（L1~L5）、稳定性评分（EWMA、Peak Hour、保底 P10/P25）与候选池发现机制作为基线。
  - 默认探针严格保持为 `ProfileCFST`（Cloudflare 官方 TCP + HTTPS），绝不自动执行 WebSocket 升级探测或向官方节点发送 `/pyway` 请求。
  - 确认不包含任何未就绪的临时 WSS 门禁逻辑、无硬编码临时域名，无任何 Cloudflare IP 前缀黑白名单。
- **改进 Windows CI 测试稳定性与跨平台探测健壮性**:
  - 修复 `TCPPing` 在 Windows 本地回环（`127.0.0.1`）耗时小于 1 微秒时返回 `0.0` 导致偶发误判为超时丢包的问题，确保成功建立 TCP 连接时返回值始终大于 0。
  - 针对 `TestDiscoveryUsesCFSTProfile` 与 `TestSeedCandidatesGOWAYWSSUsesProfile`，采用独立的 raw TCP/TLS 测试 listener 并引入 readiness pre-flight 预热，精准区分纯 TCP 探测与应用层 HTTP/TLS 请求，彻底消除 Windows 回环环境下偶发的连接竞争与 EOF 报错。
- **完善全平台自动化测试流程**:
  - 建立全平台 CI 测试流，确保 Linux（`ubuntu-latest`：单元测试、Race Detector、Go Vet）与 Windows（`windows-latest`：单元测试、Go Vet、二进制编译）均完全绿色通过。
  - 为后续独立的新版 WSS 兼容性功能开发提供可靠、干净的代码基础。

## v2.1.7 (2026-09-08)

### Release: Final Concurrency Safety Fix & Windows 11 x64 Release
- **Fixed `RouteStore.SaveSnapshot()` real concurrent data race (`history.go`, `ewma.go`)**:
  - Root cause: `SaveSnapshot()` collected live `*RouteRecord` pointers under `RLock`, released the lock, then `json.Marshal` read `Metrics/Samples/EWMA/PeakHours` while `RecordProbeResult/AddSample/Upsert` mutated them concurrently.
  - Fix: added `RouteEWMATracker.Clone()` (fresh mutex, copied under tracker `RLock`) and `RouteRecord.Clone()` (deep copies `Samples`, `RecommendationReasons`, `EWMA` snapshot, `PeakHours`), and `SaveSnapshot()` now deep-copies all unique records while holding `s.mu.RLock()`, marshaling only the isolated copy outside the lock.
  - Bumped `SnapshotContainer.Version` to `"2.1.7"` (backward-compatible load path unchanged).
- **Fixed `ProbeScheduler` / `DiscoveryManager` ctx-cancel -> restart lifecycle race (`probe.go`, `discovery.go`)**:
  - Root cause: goroutine exit via `ctx.Done()` unconditionally executed `running.Store(false)` without generation check and left stale `stopCh` behind; a stale cancel could therefore clear `running` after a newer `Start()` had set it to true.
  - Fix: `Start()` now checks `running.Load()` under `mu` and creates a new `stopCh` per generation; worker goroutines capture `(myStopCh, myCtx)` and on `myCtx.Done()` clear `running` and nil `stopCh` only when `ps.stopCh == myStopCh` / `dm.stopCh == myStopCh` under lock; `Stop()` closes and nils `stopCh`. `DiscoveryManager` immediate startup pass now captures its context parameter and skips work when already cancelled.
- **Real concurrency tests (`concurrency_race_test.go`)**:
  - Added `TestRouteStoreSaveSnapshotConcurrentRace` driving real `RecordProbeResult` writers concurrently with real `SaveSnapshot`/`GetAll`/`GetBest`/tier-evaluation readers, then verifying final snapshot reload.
  - Added `TestProbeSchedulerCtxCancelRestartNoClobber` and `TestDiscoveryManagerCtxCancelRestartNoClobber` proving old `ctx-cancel` cannot clobber a restarted generation's `running` flag.
- **Targeted Windows 11 x64 Release (`.github/workflows/release.yml`)**:
  - Publishes only `CFST-windows-amd64.exe` (`CGO_ENABLED=0`).

## v2.1.6 (2026-09-08)

### Release: Final Probe Lifecycle Hardening & Windows 11 x64 Release
- **Fixed `stopCh` Lifecycle Race (`probe.go`, `discovery.go`)**:
  - Bound local `stopCh` channel parameters directly into `go func(stopCh <-chan struct{})` for both `ProbeScheduler` and `DiscoveryManager`.
  - Goroutine select loops listen strictly to `case <-stopCh:`, eliminating dynamic evaluation races when restarting components (`Start -> Stop -> Start`).
- **Hardened In-Flight Probe Result Isolation (`probe.go`, `history.go`, `api.go`)**:
  - Added `RouteStore.GetMetrics()` and `RouteStore.GetSamples()` returning thread-safe snapshot value copies.
  - Eliminated internal `*RouteRecord.Metrics` pointer leaks in `ProbeOnce` and HTTP API routes (`/api/routes/{ip}`, `/api/routes/{ip}/samples`), ensuring zero data races under concurrent reading and probing.
- **Genuine Candidate L3 Integration Coverage (`probe_test.go`)**:
  - Rewrote `TestCandidateL3ObservationAndPromotion` to execute production `scheduler.ProbeOnce` directly with `nowFunc` time simulation across 300+ seconds.
  - Candidate promotion to Standby is driven 100% by production `RecordProbeResult`, `EWMA`, `ScoreEngine`, and `EvaluateTierTransitionsLocked`.
- **Full Goroutine Lifecycle Race Tests (`probe_test.go`, `discovery_test.go`)**:
  - Added `TestProbeSchedulerRestartNoRace` and `TestDiscoveryManagerRestartNoRace` with `onStartGoroutine` and `onExitGoroutine` hooks to verify real background worker startup, teardown, and clean recreation.
- **Legacy Snapshot Backwards Compatibility (`history.go`, `history_test.go`)**:
  - Added `TestLegacySnapshotWithoutDurationSeconds` verifying that older JSON snapshots missing `duration_seconds` load cleanly and safely use the 10.0s fallback in `AddSample()`.
  - Bumped `SnapshotContainer.Version` to `"2.1.6"`.
- **Targeted Windows 11 x64 Release (`.github/workflows/release.yml`)**:
  - Configured GitHub Actions to test on Linux runner (`go test`, `go test -race`, `go vet`) and build Windows 11 x64 binary (`CFST-windows-amd64.exe` with `CGO_ENABLED=0`).

## v2.1.5 (2026-09-08)

### Release: Harden Candidate Discovery and Probe Lifecycle
- **Fix Candidate L3 metrics propagation (`probe.go`, `score.go`)**:
  - `ExecuteLayeredProbeWithSnapshot()` properly maps L3 lightweight HTTP probe results (`Speed`, `Colo`, `TTFB`) to `LayeredProbeResult` (`SingleSpeed`, `DownloadSpeed`, `Colo`, `LoadLatency`).
  - `ProbeOnce()` updates `RouteMetrics` with L3 probe speeds while preserving existing historical statistics without fabricating false P10/stability metrics.
  - `ScoreEngine` naturally evaluates routes based on observed L3 speed and derives stability from L1 latency/jitter/loss consistency when L4 interval samples are absent, allowing valid candidates to progress to Standby.
- **Fix Discovery RouteStore synchronization (`history.go`, `discovery.go`)**:
  - Replaced direct pointer field mutations on `rec.Metrics.Colo` outside mutex locks with `RouteStore.UpdateRouteColo(ip, colo)` executing under synchronized `store.mu.Lock()`.
- **Immediate startup discovery (`discovery.go`)**:
  - `DiscoveryManager.Start()` triggers an immediate low-bandwidth discovery pass asynchronously on startup in a background goroutine, ensuring fresh IP discovery without waiting for the 60-minute ticker.
- **True concurrent in-flight probe test (`probe_test.go`)**:
  - Added `TestProbeInFlightConcurrentRace` testing multiple goroutines competing simultaneously via `startCh` and proving that `inFlight.LoadOrStore` guarantees exactly one probe execution per IP.
- **Candidate promotion integration tests (`probe_test.go`, `discovery_test.go`)**:
  - Added `TestCandidateL3ObservationAndPromotion` testing genuine Candidate progression (L1 -> L2 -> L3 -> `RecordProbeResult` -> `EWMA` -> `Score` -> `ObservationDuration` -> `EvaluateTierTransitions` -> `Standby`) without hardcoded scores.
  - Added `TestCandidateL3Observation`, `TestDiscoveryRunOnceAddsCandidate`, and `TestDiscoveryRunOncePreservesHistory`.
- **Discovery overlap protection (`discovery.go`, `discovery_test.go`)**:
  - Added atomic CAS `inProgress` guard on `DiscoveryManager.RunOnce()` preventing concurrent overlapping discovery passes.
  - Added unit test `TestDiscoveryNoOverlappingRuns`.
- **Stall duration calculation fix (`engine.go`, `history.go`, `engine_test.go`)**:
  - Updated `ProcessIntervalSamples` to accumulate actual `it.DeltaDuration` across consecutive stall intervals rather than multiplying count by current delta.
  - Added `TestLongestStallDurationVariableIntervals` validating variable stall interval accumulation.
  - Made stall rate denominator in `AddSample` use `sample.DurationSeconds` (with 10.0s legacy fallback).
- **Scheduler/Discovery restart safety (`probe.go`, `discovery.go`)**:
  - Re-initialized `stopCh` on `Start()` under lock in both `ProbeScheduler` and `DiscoveryManager`, allowing safe `Start -> Stop -> Start` lifecycles.
  - Added `TestProbeSchedulerRestart` and `TestDiscoveryManagerRestart`.

## v2.1.4 (2026-09-08)

### Feature: Continuous Discovery, In-Flight De-duplication & Candidate Promotion
- **Per-IP In-Flight Probe De-duplication (`probe.go`)**:
  - Implemented `inFlight sync.Map` on `ProbeScheduler` to ensure at most one active `ProbeOnce` runs on any given IP concurrently.
  - Guaranteed safe cleanup via `defer ps.inFlight.Delete(ip)` across all execution paths (normal completion, error, panic, context timeout).
  - Wired in-flight guard into `evaluateAndSchedule()` and `TriggerOnDemand()` to avoid unnecessary goroutine and timer overhead.
- **Fixed Configuration Snapshot in `ProbeOnce` (`probe.go`)**:
  - Completely eradicated un-synchronized field reads (`ps.cfg.Port`).
  - Added single atomic snapshot read `cfg := ps.GetConfig()` at the start of `ProbeOnce()`.
  - Pass snapshot `cfg` and resolved `target` to `ExecuteLayeredProbeWithSnapshot()`, preventing mid-probe configuration tearing when API updates occur.
- **Continuous Background Cloudflare IP Discovery (`discovery.go`, `daemon.go`, `scanner.go`)**:
  - Added `DiscoveryManager` running periodic background discovery (default interval: 60 minutes, scan count: 200 IPs).
  - Discovery is strictly low bandwidth: executes L1 TCP Ping + L2 `HTTPSConnectivityCheck` for `ProfileCFST` (no WSS, no full speed tests during scan).
  - Discovered new valid IPs enter `RouteStore` strictly as `TierCandidate` (never directly Active).
  - Re-discovered existing IPs preserve all historical records (`EWMA`, discrete samples, peak-hour stats, health, and stability score) without overwrite.
  - Structured summary logging:
    ```
    [Discovery] Scanning 200 Cloudflare IPs...
    [Discovery] TCP valid: 42 | HTTPS valid: 31 | New candidates: 12 | Existing routes: 19
    [Discovery] Best candidate: 104.x.x.x | Score: 91.4 | P10: 38.2 MB/s | Stability: 88.7
    ```
- **Candidate Promotion & Degraded Route Demotion Lifecycle (`history.go`)**:
  - Implemented `EvaluateTierTransitions()` with single-step transition protection:
    - `Candidate -> Standby`: Requires `ObservationDuration >= 300s`, `Samples >= 3`, `Confidence >= 35.0`, `Health == HealthHealthy`, `FinalScore >= 70.0`, `PacketLoss <= 0.05`, `Jitter <= 25.0`, `StallCount == 0`.
    - `Standby -> Active`: When Active route is degraded or when Standby consistently and significantly beats Active over long term (`FinalScore >= active.FinalScore + 5.0`, `Confidence >= 60.0`, `P10Speed >= active.P10Speed`, `ObservationDuration >= 900s`). Eliminates single high-speed spike takeover.
    - `Active -> Standby`: When Active route degrades (`HealthDegraded`, `HealthFailing`, `SpeedDropPercent >= 35%`, `ConsecutiveFails >= 2`).
    - `Standby -> Candidate`: When Standby fails repeatedly (`HealthFailing`, `FinalScore < 50.0`, `ConsecutiveFails >= 3`).
    - `Candidate -> Failed`: When Candidate fails persistently (`HealthFailed`, `ConsecutiveFails >= 5`).
    - `Failed Pruning`: Removes permanently dead routes (`HealthFailed`, `ConsecutiveFails >= 10`, untested > 2 hours).
- **New Discovery Status API & CLI Flags (`api.go`, `main.go`)**:
  - Added `GET /api/discovery/status` returning runtime state (`enabled`, `interval_sec`, `last_run`, `last_duration_sec`, `scanned`, `tcp_valid`, `https_valid`, `new_candidates`, `existing_routes`).
  - Added CLI flags `-discovery`, `-discovery-interval`, `-discovery-count`.
  - Bumped version to `v2.1.4` across CLI, API, Web UI, and Snapshot persistence.
- **Comprehensive Unit & Concurrency Test Suite (`discovery_test.go`, `probe_test.go`)**:
  - `TestProbeInFlightDedup`: verifies concurrency de-duplication on same IP.
  - `TestProbeUsesConfigSnapshot`: proves probe uses immutable snapshot during API updates.
  - `TestDiscoveryAddsCandidate`: verifies new nodes enter `RouteStore` strictly as `TierCandidate`.
  - `TestDiscoveryDoesNotDuplicateExistingRoute`: verifies existing route histories are preserved intact.
  - `TestDiscoveryUsesCFSTProfile`: verifies Discovery adheres strictly to CFST HTTPS profile without WSS.
  - `TestCandidatePromotion`: verifies multi-horizon candidate promotion gating.
  - `TestLongTermDemotion`: verifies staged demotion and persistent failure pruning.

## v2.1.3 (2026-09-08)

### Fix: Unify Daemon and Initial Scan with ProbeProfile Architecture
- **Daemon Profile Initialization (`daemon.go`)**:
  - Eliminated hardcoded `ProfileGOWAYWSS` override in `RunDaemon()`. Daemon now strictly builds on `DefaultProbeConfig()` (`ProfileCFST`).
  - Added `ConfigToProbeConfig()` which only modifies profile-independent scheduling parameters (`ActiveInterval`, `StandbyInterval`, `CandidateInterval`, `FailedInterval`, `QuickDuration`, `FullDuration`).
  - Abolished automatic switching to GOWAY-WSS based on non-empty `WSSHost`. Switching to GOWAY-WSS or CUSTOM now strictly requires explicit profile configuration.
  - Added explicit profile details in daemon startup banner (`Profile`, `Protocol`, `Test URL`, `SNI`, `Host`, `Path`).
- **Profile-Driven Candidate Discovery & Seeding (`scanner.go`, `daemon.go`)**:
  - Implemented `ScanRoutesWithProfile()`:
    - `ProfileCFST`: TCP Ping + `HTTPSConnectivityCheck` (strictly zero WSS checks, never requests `/pyway`).
    - `ProfileGOWAYWSS`: TCP Ping + `WSSHandshakeCheck` using `profile.Host`, `profile.SNI`, and `profile.Path`.
    - `ProfileCustom`: Protocol-driven (`HTTPSConnectivityCheck` for HTTP/HTTPS, `WSSHandshakeCheck` for WSS).
  - Updated `seedCandidates()` in `daemon.go` to use `ScanRoutesWithProfile()` with the active scheduler profile.
  - Updated `runQuickFilter()` and `runParallelDownloadTest()` to resolve targets strictly from `ProbeProfile`.
  - Retained `ScanPing()` as a legacy compatibility wrapper and fixed duplicate `done.Add(1)` progress counter.
- **Custom VPS Port & Host Header Preservation (`probe.go`, `engine.go`, `api.go`)**:
  - In `NewProfileCustom()`, `NormalizeProbeConfig()`, and `ResolveProbeTarget()`:
    - Non-default ports (e.g. `:8443`) are properly retained in HTTP `Host` header (`my-vps.com:8443`).
    - TLS SNI strictly remains the hostname without port (`my-vps.com`).
    - Default port (443) uses hostname without port in `Host`.
  - Updated `SingleStreamTestDetailed()` to accept and execute directly against `ResolvedProbeTarget`.
  - Added `LightweightHTTPProbeTarget()` driven by `ResolvedProbeTarget`.
- **API & CLI Profile Extensions (`main.go`, `api.go`)**:
  - Added `-profile` CLI flag to `main.go`.
  - Added `Port` support in `ConfigUpdateRequest` and URL port extraction in `/api/config`.
- **Comprehensive Regression Test Suite (`daemon_test.go`, `probe_test.go`)**:
  - Added `TestRunDaemonDefaultProfileIsCFST`: guarantees daemon defaults to CFST HTTPS probe.
  - Added `TestSeedCandidatesCFSTDoesNotUseWSS`: verifies candidate seeding never touches WSS in CFST mode.
  - Added `TestSeedCandidatesGOWAYWSSUsesProfile`: verifies WSS candidate seeding strictly uses profile Host/Path.
  - Added `TestCustomNonDefaultPortHost`: verifies non-default port header formatting.
  - Added `TestAPIConfigActuallyChangesExecutionProfile`: verifies API updates directly modify probe execution targets.
  - Added `TestArchitectureAntiRegression`: guards against profile override regressions across all subsystems.

## v2.1.2 (2026-09-08)

### Fix: Unify Probe Profile Execution & Isolate GOWAY WSS Mode
- **Unify Probe Profile Execution (`probe.go`)**:
  - Implemented `ResolvedProbeTarget` and `ResolveProbeTarget()` to strictly unify parameters across all probe layers (L1~L4).
  - All network probes (L2, L3, L4) are driven exclusively by `cfg.Profile` rather than legacy disparate fields (`cfg.URL`, `cfg.SNI`, `cfg.WSSHost`).
  - Added centralized `NormalizeProbeConfig()` to guarantee `cfg.Profile` is the single source of truth while keeping legacy fields in sync.
- **Isolate GOWAY WSS Mode & Fix Cloudflare Official WSS Conflict (`probe.go`, `engine.go`)**:
  - In `ProfileCFST`, removed inappropriate `WSSHandshakeCheck` calls and `/pyway` requests.
  - Implemented lightweight `HTTPSConnectivityCheck` for `ProfileCFST` (TLS handshake + HTTP HEAD/GET Range) verifying TLS status, TTFB, HTTP status, and CDN Colo without WebSocket upgrades.
  - `ProfileGOWAYWSS` specifically executes GOWAY WSS Handshakes.
  - `ProfileCustom` dynamically determines L2 checks based on protocol (`wss` vs `https`/`http`).
- **Configurable WSS Handshake Check (`engine.go`)**:
  - Updated `WSSHandshakeCheck` to accept `(ip, port, sni, host, path, timeout)`.
  - Eliminated hardcoded `/pyway` path and hardcoded Host headers. Both are fully dynamic from the profile.
- **Custom Profile URL & Header Isolation (`engine.go`, `probe.go`)**:
  - Custom VPS URLs now accurately extract and use their own hostname, SNI, Host, and Path.
  - Custom endpoints no longer default to `speed.cloudflare.com` Origin/Referer headers in `LightweightHTTPProbe`.
- **Immediate API Profile Application (`api.go`, `probe.go`)**:
  - POST `/api/config` updating `profile_type`, `test_url`, `sni`, `host`, `path` immediately affects `GlobalProbeScheduler` and the very next probe pass without delay or stale cache.

## v2.1.1 (2026-09-08)

### Hardening: Long-Term Probe Stability & Low-Traffic Scheduling
- **Probe Profile Explicitly Standardized (`probe.go`, `api.go`)**:
  - Default probe profile explicitly standardized as Cloudflare Official (`ProfileCFST`, `https://speed.cloudflare.com/__down?bytes=500000000`).
  - Added dedicated constructors `NewProfileCFST()`, `NewProfileGOWAYWSS(host, path, sni, port)`, and `NewProfileCustom(customURL, sni, port)`.
  - Exposed `profile`, `profile_type`, `test_url`, `sni`, `host`, `path`, and `protocol` in `/api/health` and `/api/config`.
- **Low-Traffic Multi-Layered Probe Scheduler (`probe.go`, `engine.go`)**:
  - L1 (TCP Ping): Minimal byte overhead (SYN/ACK).
  - L2 (WSS Handshake): Low byte overhead (TLS + HTTP 101 Handshake, 1~2KB).
  - L3 (Lightweight HTTP Probe): Low-bandwidth probe (~100KB download / HTTP Range) validating TTFB, HTTP status, and Cloudflare colo without wasting bandwidth.
  - L4 (Full Speed Calibration): Throttled schedule:
    - Active routes: L3 probe every 3 cycles (~30s), L4 full speed calibration only every 60 cycles (~10m).
    - Standby routes: L3 every 6 cycles, L4 full speed every 120 cycles.
    - Candidate routes: L1/L2 only.
    - Failed routes: strictly L1/L2 recovery checks, ZERO speed tests.
- **Observation Span & Duration-Gated Confidence (`history.go`)**:
  - Confidence calculation is strictly gated by observation time duration (span) + sample count + success rate + peak hour coverage + recency.
  - Guaranteed: 12 samples over 2 minutes cannot exceed 50% confidence. High confidence (>70%) requires multi-hour observation spans.
- **SpeedDrop Baseline Calculation Order Fix (`history.go`)**:
  - Baseline P10 is captured from EWMA *before* adding the current measurement sample.
  - Sudden speed drops (e.g., from 50 MB/s to 30 MB/s) accurately register a 40% drop rather than being smoothed out by the sample itself.
- **Peak Hour Matrix Historical EWMA Decay (`history.go`)**:
  - Day-to-day exponential decay (`math.Pow(0.60, days)`) and $\alpha=0.35$ EWMA weighting ensures recent peak data dominates over stale historical records.
- **True 24-Hour History Retention (`history.go`)**:
  - Sample capacity expanded from 1,000 to 10,000 samples, pruned by a 24-hour cutoff window anchored to latest sample timestamp.
- **Stale Route Protection (`history.go`)**:
  - Routes untested for > 5m (active) or > 15m (others) are marked `is_stale = true` and receive a 50% discount on confidence and final score.
  - Routes untested for > 60m are excluded from `GetBest()` recommendations.

## v2.1.0 (2026-09-08)

### Major Architecture Upgrade: GOWAY Route Quality Probe & Stability Analyzer
- **True 1-Second Interval Sampling (`engine.go`)**:
  - Implemented `SpeedIntervalSample` and `SpeedMetrics` tracking discrete 1s download intervals.
  - **Fixed critical bug**: Never drops 0 MB/s intervals (`speed > 0` filter removed).
  - True `MinSpeed` accurately records 0 MB/s when a stall occurs.
  - Added anti-buffering percentiles: `P10Speed`, `P25Speed`, `MedianSpeed`, `MaxSpeed`, `StdDev`, `CoefficientOfVariation`.
  - Introduced stall metrics: `StallCount`, `TotalStallDuration`, `ZeroSpeedIntervals`, `LongestStallDuration`, and `StallRate`.
- **Composite Stability Formula**:
  - Replaced naive ratio with multi-dimensional stability index:
    $\text{Stability} = 0.35 \times \text{SpeedConsistency} + 0.25 \times \text{FloorStability} + 0.20 \times \text{NoStallRatio} + 0.10 \times \text{LatencyConsistency} + 0.10 \times \text{LossConsistency}$.
- **Multi-Horizon Scoring Engine (`score.go`)**:
  - Rebalanced component weights: Speed 20%, P10 20%, MinSpeed 10%, Stability 20%, PacketLoss 10%, Jitter 10%, Latency 5%, Handshake 5%.
  - Peak mode component weights: Speed 15%, P10 25%, MinSpeed 10%, Stability 25%, PacketLoss 10%, Jitter 10%, Latency 2.5%, Handshake 2.5%.
  - Horizon weights for Normal mode: Instant 20%, Short-term 30%, Long-term 25%, Peak-hour 15%, Confidence 10%.
  - Horizon weights for Peak mode: Instant 10%, Short-term 25%, Long-term 25%, Peak-hour 25%, Confidence 15%.
  - Guarantees rock-solid 55 MB/s node outranks 100 MB/s peak node with stalls.
- **Degradation & Health State Machine (`metrics.go`)**:
  - Added anti-jitter debouncing: requires 3 consecutive degraded occurrences to switch to `DEGRADED`, and 3 consecutive successes to restore `HEALTHY`.
  - Added `StabilityGrade` (`STABLE`, `FLUCTUATING`, `DEGRADED`, `UNSTABLE`, `FAILED`).
  - Added `RouteRecommendation` (`BEST`, `GOOD`, `USABLE`, `DEGRADED`, `AVOID`, `FAILED`) and human/machine-readable `recommendation_reasons`.
- **EWMA Failure Pressure & Decay (`ewma.go`)**:
  - Added `RecordFailure(now)`: applies failure degradation pressure, forcing exponential decay of EWMA speeds so dead routes immediately lose top ranking.
- **Route Selection & Atomic Persistence (`history.go`)**:
  - Upgraded `GetBest`: filters out `FAILED`/`FAILING`, applies 25% discount to `DEGRADED` and 30% to `RECOVERING`, prioritizes `PeakHourScore` during peak hours, and resolves ties using multi-level tie-breaking (`EffectiveScore` > `P10Speed` > `Jitter` > `PacketLoss` > `RTT`).
  - Fixed nested lock deadlock bug in `RouteStore`.
  - Added atomic Windows-safe snapshot saving with `.tmp` write, `.bak` rotation, and backward-compatible schema loading.
- **REST API Subroutes & Enriched Payload (`api.go`)**:
  - Subroutes: `/api/routes/{ip}/history`, `/api/routes/{ip}/peak`, `/api/routes/{ip}/samples`.
  - Enriched JSON payloads with `speed.p10`, `speed.median`, `speed.avg`, `speed.min`, `stalls`, `stability_grade`, `recommendation`, `reasons`.
- **Web UI & CLI Upgrades (`index.html`, `scanner.go`, `main.go`)**:
  - Updated to v2.1.0, displaying P10 speed, stall counts, recommendation badges, and CSV export with P10 columns.
  - Added `-v` / `-version` flag in CLI.


### Enhancements
- **EWMA MinSpeed Integration**: Added `MinSpeed` tracking into both short-term and long-term EWMA snapshots (`RouteEWMATracker`), providing direct buffer-underrun indicators for continuous video streaming scenarios.
- **Scoring Engine Polish**: Updated `ShortTermScore` and `LongTermScore` to incorporate smoothed `EWMA MinSpeed` instead of raw instantaneous minimums.
- **Version Bump**: Updated version string across CLI banner, Web UI header, Daemon banner, and REST API (`/api/health`).

## v2.0.0 (2026-09-07)

### Major Architecture Upgrade: Route Quality Probe
- **Architectural positioning**: Upgraded CFST from a one-time speedtest tool to a continuous Route Quality Probe (`Route Quality Probe / 线路质量检测器`). Strict separation of concerns: CFST measures and emits `RouteMetrics`, while GoPass makes routing decisions and GOWAY provides high-speed tunneling.
- **Unified RouteMetrics**: Structured data model output containing IP, Port, Colo, RTT, PacketLoss, Jitter, DownloadSpeed, SingleSpeed, MinSpeed, Stability, LoadLatency, HandshakeSuccess, Multi-horizon Scores, EWMA metrics, and Health state.
- **Dynamic Multi-Horizon Scoring**:
  - `InstantScore`: single-measurement instant quality.
  - `ShortTermScore`: smoothed short-term EWMA (~5-15 min window).
  - `LongTermScore`: long-term baseline minus peak-hour historical penalties.
  - `FinalScore = Instant*0.40 + ShortTerm*0.35 + LongTerm*0.25` (configurable weights).
  - Configurable Normal Mode vs Peak Mode presets.
- **EWMA (Exponential Weighted Moving Average)**: Dual-horizon EWMA for Speed, Latency, Loss, Jitter, and Stability, eliminating transient fluctuation bias without storing infinite raw data.
- **Multi-Window History & 24h Peak Hour Matrix**:
  - In-memory real-time sliding windows (5m, 15m, 1h, 6h, 24h).
  - 24-hour dimension statistics (00..23) tracking failure rates, min speed, and average throughput across diurnal cycles (learning evening congestion patterns).
  - Periodic background state snapshot persistence (`cfst_state.json`).
- **Route Health State Machine**:
  - `HEALTHY`: high quality, low jitter, zero packet loss.
  - `DEGRADED`: speed drop >30% from baseline, or packet loss >15%, or jitter >25ms.
  - `FAILING`: 1-2 consecutive probe failures.
  - `FAILED`: 3+ consecutive failures.
  - `RECOVERING`: requires 3 consecutive successful recovery probes before re-entering HEALTHY.
- **Tiered Background Probe Scheduler**:
  - Tier-based scheduling: Active (high-frequency), Standby (medium-frequency), Candidate (low-frequency), Failed (recovery probes).
  - 5-layer probe pipeline: L1 TCP Ping -> L2 WSS Handshake -> L3 Quick Speed -> L4 Full Speed -> L5 Load Latency.
  - Bandwidth protection: strictly limits concurrent speed tests (max 1) to avoid test traffic interfering with actual user network quality.
- **Local GoPass REST API (`127.0.0.1:9876`)**:
  - `GET /api/health`: service health, uptime, score mode, route counts.
  - `GET /api/routes`: filtered route listing with RouteMetrics.
  - `GET /api/routes/best`: top-ranked healthy routes.
  - `GET /api/routes/metrics`: ultra-lightweight summary array for high-frequency GoPass polling.
  - `GET /api/routes/{ip}`: full history, EWMA, and Peak Hour matrix for a route.
  - `POST /api/routes/tier`: GoPass updates route tier (ACTIVE / STANDBY).
  - `POST /api/probe`: trigger immediate on-demand probe.
  - `GET /api/config` & `POST /api/config`: dynamic tuning of weights, modes, and intervals.
- **100% Backward Compatibility**:
  - Original CLI workflow (`cfst.exe`) preserved.
  - Web UI (`http://127.0.0.1:9876`) and SSE live testing preserved.
  - CSV export (`result_colo.csv`) preserved.
- **New CLI Flags**:
  - `-daemon`: Run as continuous Route Quality Probe daemon.
  - `-api-addr`: Custom API bind address (default `127.0.0.1:9876`).
  - `-active-interval`, `-standby-interval`, `-candidate-interval`, `-failed-interval`: Configurable probe intervals in seconds.
  - `-mode`: Scoring mode (`normal` or `peak`).
  - `-state`: State cache JSON file path.

## v1.8.6 (2026-08-25)

### Features

- **goway wss handshake verification (enabled by default)**: during the ping
  scan, each candidate IP is verified with a real TLS WebSocket upgrade
  handshake against the goway fake host (`colo.4467107.xyz`). IPs that fail the
  handshake (e.g. Cloudflare 403) are filtered out automatically, so scan
  results only contain IPs that work with goway.
- New `-wsshost` flag to customize or disable the check (pass `-wsshost=""`
  to fall back to plain TCP ping behavior).

### Notes

- The default wss fake host is `colo.4467107.xyz`; override it with
  `-wsshost=<your-host>` if your goway upstream path/host differs.

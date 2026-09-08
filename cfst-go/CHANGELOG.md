# Changelog

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

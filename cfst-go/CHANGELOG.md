# Changelog

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

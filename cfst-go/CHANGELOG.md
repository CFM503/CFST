# Changelog

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

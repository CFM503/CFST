package main

import (
	"flag"
	"os"
	"strings"
)

func main() {
	cfg := DefaultConfig()

	flag.IntVar(&cfg.Port, "p", cfg.Port, "Target port")
	flag.IntVar(&cfg.MaxScan, "max", cfg.MaxScan, "Max IPs to scan")
	flag.IntVar(&cfg.TopN, "topn", cfg.TopN, "Top N candidates by latency for speed test")
	flag.IntVar(&cfg.DLConc, "dlc", cfg.DLConc, "Parallel download test concurrency")
	flag.IntVar(&cfg.DownloadNum, "dn", cfg.DownloadNum, "Download test count")
	flag.IntVar(&cfg.Duration, "dt", cfg.Duration, "Download duration (seconds)")
	flag.Float64Var(&cfg.StopThreshold, "st", cfg.StopThreshold, "Stop threshold MB/s (CF URL mode only)")
	flag.BoolVar(&cfg.Unique, "u", cfg.Unique, "Unique C-subnet")
	flag.StringVar(&cfg.IPFile, "f", cfg.IPFile, "Custom IP file")
	flag.StringVar(&cfg.Output, "o", cfg.Output, "Output file")
	flag.IntVar(&cfg.ScanConcurrent, "sc", cfg.ScanConcurrent, "Scan concurrency")
	flag.BoolVar(&cfg.Skip429, "skip429", cfg.Skip429, "Discard 429 rate-limited IPs silently")
	flag.StringVar(&cfg.URL, "url", cfg.URL, "Custom download test URL")
	flag.IntVar(&cfg.QuickDuration, "qd", cfg.QuickDuration, "Quick pre-filter duration in seconds (custom URL mode)")
	flag.StringVar(&cfg.FilterMode, "filter", cfg.FilterMode, "Candidate filter mode (speed, multi-colo, none)")
	flag.StringVar(&cfg.SNI, "sni", cfg.SNI, "Custom TLS SNI (ServerName)")
	flag.StringVar(&cfg.WSSHost, "wsshost", cfg.WSSHost, "WebSocket fake Host for goway handshake check (enabled by default, pass empty string to disable)")
	flag.StringVar(&cfg.Profile, "profile", cfg.Profile, "Probe profile: CFST (default), GOWAY-WSS, CUSTOM")
	flag.BoolVar(&cfg.DaemonMode, "daemon", cfg.DaemonMode, "Run as continuous Route Quality Probe daemon")
	flag.StringVar(&cfg.APIAddr, "api-addr", cfg.APIAddr, "Local API listen address (default 127.0.0.1:9876)")
	flag.IntVar(&cfg.ActiveInterval, "active-interval", cfg.ActiveInterval, "Active route probe interval in seconds")
	flag.IntVar(&cfg.StandbyInterval, "standby-interval", cfg.StandbyInterval, "Standby route probe interval in seconds")
	flag.IntVar(&cfg.CandidateInterval, "candidate-interval", cfg.CandidateInterval, "Candidate route probe interval in seconds")
	flag.IntVar(&cfg.FailedInterval, "failed-interval", cfg.FailedInterval, "Failed route recovery probe interval in seconds")
	flag.StringVar(&cfg.ScoreMode, "mode", cfg.ScoreMode, "Scoring mode: normal or peak")
	flag.StringVar(&cfg.StateFile, "state", cfg.StateFile, "State persistence JSON file path")
	flag.BoolVar(&cfg.DiscoveryEnabled, "discovery", cfg.DiscoveryEnabled, "Enable continuous Cloudflare IP discovery in daemon mode")
	flag.IntVar(&cfg.DiscoveryInterval, "discovery-interval", cfg.DiscoveryInterval, "Discovery scan interval in seconds (default 3600)")
	flag.IntVar(&cfg.DiscoveryScanCount, "discovery-count", cfg.DiscoveryScanCount, "Discovery IPs to scan per pass (default 200)")
	versionFlag := flag.Bool("v", false, "Show version and exit")
	flag.BoolVar(versionFlag, "version", false, "Show version and exit")

	webMode := false
	webPort := "9876"
	if len(os.Args) > 0 {
		var newArgs []string
		newArgs = append(newArgs, os.Args[0])
		for i := 1; i < len(os.Args); i++ {
			if os.Args[i] == "-web" {
				webMode = true
				if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
					webPort = os.Args[i+1]
					i++
				}
			} else {
				newArgs = append(newArgs, os.Args[i])
			}
		}
		os.Args = newArgs
	}

	flag.Bool("web", false, "Start Web UI server (-web <port>)")
	flag.Parse()

	if *versionFlag {
		println("CFST v2.1.7 (GOWAY Route Quality Probe & Long-Term Stability Analyzer)")
		return
	}

	if cfg.DaemonMode {
		RunDaemon(cfg)
	} else if webMode {
		cfg.WebMode = true
		cfg.WebPort = webPort
		if !strings.Contains(cfg.WebPort, ":") {
			cfg.WebPort = ":" + cfg.WebPort
		}
		RunWeb(cfg)
	} else {
		RunCLI(cfg)
	}
}

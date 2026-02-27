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
	flag.IntVar(&cfg.Conc, "c", cfg.Conc, "Download threads per IP")
	flag.IntVar(&cfg.DownloadNum, "dn", cfg.DownloadNum, "Download test count")
	flag.IntVar(&cfg.Duration, "dt", cfg.Duration, "Download duration (seconds)")
	flag.Float64Var(&cfg.StopThreshold, "st", cfg.StopThreshold, "Stop threshold MB/s")
	flag.BoolVar(&cfg.Unique, "u", cfg.Unique, "Unique C-subnet")
	flag.StringVar(&cfg.IPFile, "f", cfg.IPFile, "Custom IP file")
	flag.StringVar(&cfg.Output, "o", cfg.Output, "Output file")
	flag.IntVar(&cfg.ScanConcurrent, "sc", cfg.ScanConcurrent, "Scan concurrency")
	flag.BoolVar(&cfg.Skip429, "skip429", cfg.Skip429, "Discard 429 rate-limited IPs silently and find replacements")
	flag.StringVar(&cfg.URL, "url", cfg.URL, "Custom download test URL (bypass 429 block)")

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

	if webMode {
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

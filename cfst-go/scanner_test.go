package main

import (
	"strings"
	"testing"
)

func TestResolveSpeedTestTargetForGOWAYWSS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profile = "GOWAY-WSS"
	cfg.WSSHost = "dedi.4467107.xyz"
	cfg.WSSPath = "/pyway"
	cfg.SNI = "dedi.4467107.xyz"
	cfg.Port = 443
	target := resolveSpeedTestTarget(cfg, "192.0.2.10")
	if target.ProfileType != ProfileCFST {
		t.Fatalf("expected ProfileCFST, got %s", target.ProfileType)
	}
	if target.Host != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com Host, got %q", target.Host)
	}
	if target.SNI != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com SNI, got %q", target.SNI)
	}
	if target.Path != "/__down" {
		t.Fatalf("expected /__down Path, got %q", target.Path)
	}
	if !strings.EqualFold(target.Protocol, "https") {
		t.Fatalf("expected https protocol, got %q", target.Protocol)
	}
	if target.Port != 443 {
		t.Fatalf("expected port 443, got %d", target.Port)
	}
}

func TestResolveSpeedTestTargetForCustomWSS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profile = "CUSTOM"
	cfg.URL = "wss://custom.example.com:443/custom-wss"
	cfg.SNI = "custom.example.com"
	cfg.Port = 443
	target := resolveSpeedTestTarget(cfg, "192.0.2.20")
	if target.ProfileType != ProfileCFST {
		t.Fatalf("expected ProfileCFST, got %s", target.ProfileType)
	}
	if target.Host != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com Host, got %q", target.Host)
	}
	if target.SNI != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com SNI, got %q", target.SNI)
	}
	if target.Path != "/__down" {
		t.Fatalf("expected /__down Path, got %q", target.Path)
	}
	if !strings.EqualFold(target.Protocol, "https") {
		t.Fatalf("expected https protocol, got %q", target.Protocol)
	}
	if target.Port != 443 {
		t.Fatalf("expected port 443, got %d", target.Port)
	}
}

func TestResolveSpeedTestTargetKeepsCFST(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profile = "CFST"
	cfg.Port = 443
	target := resolveSpeedTestTarget(cfg, "192.0.2.30")
	if target.ProfileType != ProfileCFST {
		t.Fatalf("expected ProfileCFST, got %s", target.ProfileType)
	}
	if target.Host != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com Host, got %q", target.Host)
	}
	if target.SNI != "speed.cloudflare.com" {
		t.Fatalf("expected speed.cloudflare.com SNI, got %q", target.SNI)
	}
	if target.Path != "/__down" {
		t.Fatalf("expected /__down Path, got %q", target.Path)
	}
	if !strings.EqualFold(target.Protocol, "https") {
		t.Fatalf("expected https protocol, got %q", target.Protocol)
	}
	if target.Port != 443 {
		t.Fatalf("expected port 443, got %d", target.Port)
	}
}

func TestFinalRouteRoleSelection(t *testing.T) {
	results := []NodeResult{
		{IP: "192.0.2.1", Score: 100},
		{IP: "192.0.2.2", Score: 90},
		{IP: "192.0.2.3", Score: 80},
		{IP: "192.0.2.4", Score: 70},
		{IP: "192.0.2.5", Score: 60},
	}
	roleOf := func(i int) string {
		return routeRole(i)
	}
	if roleOf(0) != "ACTIVE" {
		t.Fatalf("index 0 must be ACTIVE")
	}
	if roleOf(1) != "STANDBY" {
		t.Fatalf("index 1 must be STANDBY")
	}
	if roleOf(2) != "STANDBY" {
		t.Fatalf("index 2 must be STANDBY")
	}
	if roleOf(3) != "CANDIDATE" {
		t.Fatalf("index 3 must be CANDIDATE")
	}
	if roleOf(4) != "CANDIDATE" {
		t.Fatalf("index 4 must be CANDIDATE")
	}

	for i := range results {
		switch i {
		case 0:
			if roleOf(i) != "ACTIVE" {
				t.Fatalf("results[0] must be ACTIVE")
			}
		case 1, 2:
			if roleOf(i) != "STANDBY" {
				t.Fatalf("results[%d] must be STANDBY", i)
			}
		default:
			if roleOf(i) != "CANDIDATE" {
				t.Fatalf("results[%d] must be CANDIDATE", i)
			}
		}
	}
}

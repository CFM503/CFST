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

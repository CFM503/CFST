package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunDaemonDefaultProfileIsCFST verifies that daemon initialization defaults strictly to ProfileCFST.
func TestRunDaemonDefaultProfileIsCFST(t *testing.T) {
	cfg := DefaultConfig()
	probeCfg := ConfigToProbeConfig(cfg)

	if probeCfg.Profile.Type != ProfileCFST {
		t.Fatalf("expected Profile.Type == ProfileCFST, got %s", probeCfg.Profile.Type)
	}
	if probeCfg.Profile.Protocol != "https" {
		t.Fatalf("expected Profile.Protocol == https, got %s", probeCfg.Profile.Protocol)
	}
	if probeCfg.Profile.SNI != "speed.cloudflare.com" {
		t.Fatalf("expected Profile.SNI == speed.cloudflare.com, got %s", probeCfg.Profile.SNI)
	}
	if probeCfg.Profile.Host != "speed.cloudflare.com" {
		t.Fatalf("expected Profile.Host == speed.cloudflare.com, got %s", probeCfg.Profile.Host)
	}
	if probeCfg.WSSHost != "" {
		t.Fatalf("expected empty WSSHost in CFST mode, got %s", probeCfg.WSSHost)
	}
}

// TestSeedCandidatesCFSTDoesNotUseWSS proves that CFST candidate discovery scan does NOT invoke WSS handshake or request /pyway.
func TestSeedCandidatesCFSTDoesNotUseWSS(t *testing.T) {
	var wssAttempted atomic.Bool
	var httpsAttempted atomic.Bool

	// Start a test server simulating a route target
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" || r.URL.Path == "/pyway" {
				wssAttempted.Store(true)
			}
			httpsAttempted.Store(true)
			w.Header().Set("cf-ray", "12345-SJC")
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()

	profile := NewProfileCFST()
	profile.Port = port
	profile.TestURL = fmt.Sprintf("http://127.0.0.1:%d/__down", port)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, port, 1, profile, nil)

	if wssAttempted.Load() {
		t.Fatalf("CFST mode must NEVER attempt WebSocket handshake or request /pyway during candidate seeding!")
	}
}

// TestSeedCandidatesGOWAYWSSUsesProfile proves that only ProfileGOWAYWSS invokes WSS with configured Host and Path.
func TestSeedCandidatesGOWAYWSSUsesProfile(t *testing.T) {
	var wssAttempted atomic.Bool
	var mu sync.Mutex
	var wssReceivedHost, wssReceivedPath string

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create test certificate: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
	})
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "closed") {
					return
				}
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				tlsConn, ok := c.(*tls.Conn)
				if !ok {
					return
				}
				br := bufio.NewReader(tlsConn)
				reqLine, err := br.ReadString('\n')
				if err != nil {
					return
				}
				parts := strings.Split(strings.TrimSpace(reqLine), " ")
				reqPath := ""
				if len(parts) >= 2 {
					reqPath = parts[1]
				}
				host, upgrade := "", ""
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						break
					}
					if idx := strings.Index(trimmed, ":"); idx > 0 {
						k := strings.ToLower(strings.TrimSpace(trimmed[:idx]))
						v := strings.TrimSpace(trimmed[idx+1:])
						switch k {
						case "host":
							host = v
						case "upgrade":
							upgrade = v
						}
					}
				}
				if strings.ToLower(upgrade) == "websocket" {
					wssAttempted.Store(true)
					mu.Lock()
					wssReceivedHost = host
					wssReceivedPath = reqPath
					mu.Unlock()
				}
				_, _ = tlsConn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
			}(conn)
		}
	}()

	prof := NewProfileGOWAYWSS("edge.goway.custom", "/custom-pyway", "sni.goway.custom", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, port, 1, prof, nil)

	if !wssAttempted.Load() {
		t.Fatalf("expected WSS handshake to be attempted for ProfileGOWAYWSS")
	}
	mu.Lock()
	gotHost, gotPath := wssReceivedHost, wssReceivedPath
	mu.Unlock()
	if gotHost != "edge.goway.custom" {
		t.Fatalf("expected WSS host edge.goway.custom, got %s", gotHost)
	}
	if gotPath != "/custom-pyway" {
		t.Fatalf("expected WSS path /custom-pyway, got %s", gotPath)
	}
}

// TestCustomNonDefaultPortHost verifies non-default port is properly maintained in Host header while SNI is clean.
func TestCustomNonDefaultPortHost(t *testing.T) {
	prof := NewProfileCustom("https://example.com:8443/test.bin", "", 0)
	cfg := ProbeConfig{Profile: prof}
	NormalizeProbeConfig(&cfg)
	target := ResolveProbeTarget(cfg, "1.2.3.4", 0)

	if target.Host != "example.com:8443" {
		t.Fatalf("expected Host example.com:8443, got %s", target.Host)
	}
	if target.SNI != "example.com" {
		t.Fatalf("expected SNI example.com, got %s", target.SNI)
	}
	if target.Port != 8443 {
		t.Fatalf("expected Port 8443, got %d", target.Port)
	}
	if target.Path != "/test.bin" {
		t.Fatalf("expected Path /test.bin, got %s", target.Path)
	}

	// Also verify default 443 does not append port to Host
	prof443 := NewProfileCustom("https://example.com/test.bin", "", 443)
	cfg443 := ProbeConfig{Profile: prof443}
	NormalizeProbeConfig(&cfg443)
	target443 := ResolveProbeTarget(cfg443, "1.2.3.4", 443)
	if target443.Host != "example.com" {
		t.Fatalf("expected default 443 Host example.com, got %s", target443.Host)
	}
	if target443.SNI != "example.com" {
		t.Fatalf("expected SNI example.com, got %s", target443.SNI)
	}
}

// TestAPIConfigActuallyChangesExecutionProfile ensures API config changes directly affect probe execution target.
func TestAPIConfigActuallyChangesExecutionProfile(t *testing.T) {
	// 1. Initial state: scheduler has ProfileCFST
	origCfg := GlobalProbeScheduler.GetConfig()
	defer GlobalProbeScheduler.UpdateConfig(origCfg)

	GlobalProbeScheduler.UpdateConfig(DefaultProbeConfig())

	// 2. Call handleAPIConfig via POST
	reqBody := map[string]interface{}{
		"profile_type": "CUSTOM",
		"test_url":     "https://api-changed-vps.com:9443/dl.bin",
		"sni":          "api-changed-vps.com",
		"host":         "api-changed-vps.com:9443",
	}
	bodyBytes, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleAPIConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Verify scheduler config has changed
	cfg := GlobalProbeScheduler.GetConfig()
	if cfg.Profile.Type != ProfileCustom {
		t.Fatalf("expected ProfileCustom, got %s", cfg.Profile.Type)
	}

	// 4. Verify resolved probe execution target strictly matches updated Profile
	target := ResolveProbeTarget(cfg, "2.2.2.2", 0)
	if target.URL != "https://api-changed-vps.com:9443/dl.bin" {
		t.Fatalf("expected URL https://api-changed-vps.com:9443/dl.bin, got %s", target.URL)
	}
	if target.Host != "api-changed-vps.com:9443" {
		t.Fatalf("expected Host api-changed-vps.com:9443, got %s", target.Host)
	}
	if target.SNI != "api-changed-vps.com" {
		t.Fatalf("expected SNI api-changed-vps.com, got %s", target.SNI)
	}
	if target.Port != 9443 {
		t.Fatalf("expected Port 9443, got %d", target.Port)
	}
	if target.Protocol != "https" {
		t.Fatalf("expected Protocol https, got %s", target.Protocol)
	}
}

// TestArchitectureAntiRegression verifies that default configs throughout daemon, scanner, and scheduler strictly stay on ProfileCFST.
func TestArchitectureAntiRegression(t *testing.T) {
	// A. DefaultProbeConfig must be ProfileCFST
	dpc := DefaultProbeConfig()
	if dpc.Profile.Type != ProfileCFST || dpc.Profile.Protocol != "https" || dpc.WSSHost != "" {
		t.Fatalf("DefaultProbeConfig regression: %+v", dpc)
	}

	// B. ConfigToProbeConfig with DefaultConfig() must be ProfileCFST
	c2p := ConfigToProbeConfig(DefaultConfig())
	if c2p.Profile.Type != ProfileCFST || c2p.Profile.Protocol != "https" || c2p.WSSHost != "" {
		t.Fatalf("ConfigToProbeConfig(DefaultConfig()) regression: %+v", c2p)
	}

	// C. Even if legacy WSSHost is non-empty, ConfigToProbeConfig MUST remain ProfileCFST unless Profile is explicitly GOWAY-WSS
	cWithWSSHost := DefaultConfig()
	cWithWSSHost.WSSHost = "colo.4467107.xyz"
	cWithWSSHost.Profile = ""
	pWithWSSHost := ConfigToProbeConfig(cWithWSSHost)
	if pWithWSSHost.Profile.Type != ProfileCFST {
		t.Fatalf("WSSHost must NOT trigger GOWAY-WSS automatically! Got: %s", pWithWSSHost.Profile.Type)
	}

	// D. Explicit Profile selection works as intended
	cGoway := DefaultConfig()
	cGoway.Profile = "GOWAY-WSS"
	pGoway := ConfigToProbeConfig(cGoway)
	if pGoway.Profile.Type != ProfileGOWAYWSS || pGoway.Profile.Protocol != "wss" {
		t.Fatalf("Explicit GOWAY-WSS failed: %+v", pGoway)
	}

	cCustom := DefaultConfig()
	cCustom.Profile = "CUSTOM"
	cCustom.URL = "https://my-vps.org:8443/file.bin"
	pCustom := ConfigToProbeConfig(cCustom)
	if pCustom.Profile.Type != ProfileCustom || pCustom.Profile.Port != 8443 {
		t.Fatalf("Explicit CUSTOM failed: %+v", pCustom)
	}

	// E. DefaultConfig().GetProbeProfile() must default to ProfileCFST
	if DefaultConfig().GetProbeProfile().Type != ProfileCFST {
		t.Fatalf("DefaultConfig().GetProbeProfile() must be ProfileCFST")
	}
}

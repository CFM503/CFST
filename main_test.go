package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/csv"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key gen: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost", "vps.example.com", "speed.cloudflare.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  priv,
		}},
		NextProtos: []string{"http/1.1"},
	}
}

func TestGenerateIPs_AllSubnetsAndSafeIPv6(t *testing.T) {
	// IPv6 CIDR parsing must return nil safely without panicking
	if got := parseCIDRCached("2606:4700::/32"); got != nil {
		t.Fatalf("expected nil for IPv6 CIDR, got %+v", got)
	}

	// Empty custom file fallback check with all-subnet scan (maxScan=0, unique=true)
	ips := GenerateIPs(0, true, "")
	if len(ips) < 5000 {
		t.Fatalf("expected >= 5000 /24 subnets across CloudflareIPv4Ranges, got %d", len(ips))
	}

	seen24 := make(map[string]bool, len(ips))
	for _, ip := range ips {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			t.Fatalf("generated invalid IPv4: %q", ip)
		}
		if parsed[0] == 131 && parsed[1] == 0 && parsed[2] >= 72 && parsed[2] <= 75 {
			t.Fatalf("excluded range 131.0.72.0/22 leaked into output: %s", ip)
		}
		sub := fmt.Sprintf("%d.%d.%d", parsed[0], parsed[1], parsed[2])
		if seen24[sub] {
			t.Fatalf("duplicate /24 subnet %s in unique mode", sub)
		}
		seen24[sub] = true
	}

	// Partial sample must return requested count without /24 collisions
	sample := GenerateIPs(300, true, "")
	if len(sample) != 300 {
		t.Fatalf("expected 300 sampled IPs, got %d", len(sample))
	}
	sampleSeen := make(map[string]bool, len(sample))
	for _, ip := range sample {
		sub := subnet24Key(ip)
		if sampleSeen[sub] {
			t.Fatalf("duplicate /24 %s in sampled output", sub)
		}
		sampleSeen[sub] = true
	}
}

func TestSelectDiverseCandidates_StratifiedAcrossColoAndPrefix(t *testing.T) {
	var nodes []NodeResult
	// 40 low-RTT nodes clustered in HKG + 104.16.0.0/20
	for i := 0; i < 40; i++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("104.16.%d.5", i%16),
			TCPLatency: 25.0 + float64(i)*0.2,
			Colo:       "HKG",
		})
	}
	// 10 slightly higher-RTT nodes in SJC + 172.64.0.0/20
	for i := 0; i < 10; i++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("172.64.%d.8", i),
			TCPLatency: 55.0 + float64(i)*0.5,
			Colo:       "SJC",
		})
	}
	// 10 slightly higher-RTT nodes in NRT + 162.159.0.0/20
	for i := 0; i < 10; i++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("162.159.%d.10", i),
			TCPLatency: 60.0 + float64(i)*0.5,
			Colo:       "NRT",
		})
	}

	selected := selectDiverseCandidates(nodes, 15)
	if len(selected) != 15 {
		t.Fatalf("expected 15 selected candidates, got %d", len(selected))
	}

	coloSeen := map[string]int{}
	for _, s := range selected {
		coloSeen[s.Colo]++
	}
	if coloSeen["HKG"] == 0 || coloSeen["SJC"] == 0 || coloSeen["NRT"] == 0 {
		t.Fatalf("expected all 3 Colo/prefix buckets represented, got distribution: %+v", coloSeen)
	}
}

func TestHTTP403ZeroBackoffAndExclusion(t *testing.T) {
	// Local TLS server returning HTTP 403 Forbidden
	srv403 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("403 Forbidden"))
	}))
	defer srv403.Close()

	_, portStr, err := net.SplitHostPort(srv403.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	target := ResolvedProbeTarget{
		ProfileType: ProfileCustom,
		IP:          "127.0.0.1",
		Port:        port,
		SNI:         "localhost",
		Host:        srv403.Listener.Addr().String(),
		Path:        "/test",
		URL:         srv403.URL + "/test",
		Protocol:    "https",
	}

	sm := SingleStreamTestDetailed(context.Background(), target, 3, nil, 10, 1, 0)
	if !sm.Aborted403 || sm.HTTPStatus != 403 || sm.AverageSpeed != 0 {
		t.Fatalf("expected Aborted403=true, HTTPStatus=403, Speed=0; got %+v", sm)
	}

	// Ensure runParallelDownloadTest skips multiple 403 IPs cleanly with zero exponential cooldown backoff
	cfg := DefaultConfig()
	cfg.Port = port
	cfg.Profile = "CUSTOM"
	cfg.URL = srv403.URL + "/test"
	cfg.SNI = "localhost"
	cfg.DLConc = 1
	cfg.Duration = 2
	cfg.DownloadNum = 2

	cands := make([]NodeResult, 5)
	for i := range cands {
		cands[i] = NodeResult{IP: "127.0.0.1", Port: port, TCPLatency: 5}
	}

	start := time.Now()
	res := runParallelDownloadTest(context.Background(), cands, cfg, nil, nil, nil, nil)
	elapsed := time.Since(start)
	if len(res) != 0 {
		t.Fatalf("expected 0 valid results when all nodes return 403, got %d", len(res))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("403 zero-backoff took too long (%v); possible unintended 429 cooldown", elapsed)
	}

	// Ensure RouteMetrics / ScoreEngine treat Colo="403" as non-healthy and penalize
	rm := FromNodeResult(NodeResult{IP: "1.2.3.4", Colo: "403", DownloadSpeed: 20}, TierCandidate)
	if rm.HandshakeSuccess || rm.Health == HealthHealthy {
		t.Fatalf("expected Colo=403 node to mark HandshakeSuccess=false & unhealthy, got %+v", rm)
	}
}

func TestConfigUnificationAndGowayShorthand(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GowayTarget = "https://vps.example.com:8443/customws"
	resolveGowayFields(&cfg)

	cliProf := cfg.GetProbeProfile()
	daemonCfg := ConfigToProbeConfig(cfg)

	if cliProf.Type != ProfileGOWAYWSS || daemonCfg.Profile.Type != ProfileGOWAYWSS {
		t.Fatalf("expected both CLI and Daemon profiles to be GOWAY-WSS, got cli=%s daemon=%s", cliProf.Type, daemonCfg.Profile.Type)
	}
	if cliProf.Host != "vps.example.com" || daemonCfg.Profile.Host != "vps.example.com" {
		t.Fatalf("host mismatch: cli=%q daemon=%q", cliProf.Host, daemonCfg.Profile.Host)
	}
	if cliProf.SNI != "vps.example.com" || daemonCfg.Profile.SNI != "vps.example.com" {
		t.Fatalf("sni mismatch: cli=%q daemon=%q", cliProf.SNI, daemonCfg.Profile.SNI)
	}
	if cliProf.Path != "/customws" || daemonCfg.Profile.Path != "/customws" {
		t.Fatalf("path mismatch: cli=%q daemon=%q", cliProf.Path, daemonCfg.Profile.Path)
	}
	if cliProf.Port != 8443 || daemonCfg.Profile.Port != 8443 {
		t.Fatalf("port mismatch: cli=%d daemon=%d", cliProf.Port, daemonCfg.Profile.Port)
	}

	// Also verify -sni domain/path parity
	sniCfg := DefaultConfig()
	sniCfg.SNI = "edge.example.com/pyway"
	resolveGowayFields(&sniCfg)
	if sniCfg.Profile != "GOWAY-WSS" || sniCfg.WSSHost != "edge.example.com" || sniCfg.WSSPath != "/pyway" {
		t.Fatalf("unexpected -sni domain/path resolution: %+v", sniCfg)
	}

	// Verify interactive wizard saves and reuses last config cleanly in temp dir
	lastFile := filepath.Join(t.TempDir(), ".cfst_last.json")
	wCfg := DefaultConfig()
	var out bytes.Buffer
	// Choose mode [1] GOWAY, enter domain vps.example.com, default path "", Fast preset "1"
	input := "1\nvps.example.com\n\n1\n"
	web := runInteractiveWizardWithIO(&wCfg, strings.NewReader(input), &out, lastFile)
	if web || wCfg.Profile != "GOWAY-WSS" || wCfg.WSSHost != "vps.example.com" || wCfg.WSSPath != "/pyway" || wCfg.Preset != "fast" {
		t.Fatalf("unexpected interactive wizard config: web=%v cfg=%+v", web, wCfg)
	}

	// Second run presses Enter once ("") to reuse saved config
	wCfg2 := DefaultConfig()
	web2 := runInteractiveWizardWithIO(&wCfg2, strings.NewReader("\n"), &out, lastFile)
	if web2 || wCfg2.WSSHost != "vps.example.com" || wCfg2.WSSPath != "/pyway" || wCfg2.Preset != "fast" {
		t.Fatalf("expected 1-Enter reuse of last config, got %+v", wCfg2)
	}
}

func TestResolveSpeedTargetForProbe(t *testing.T) {
	probeCfg := DefaultProbeConfig()
	probeCfg.Profile = NewProfileGOWAYWSS("vps.example.com", "/pyway", "vps.example.com", 8443)
	NormalizeProbeConfig(&probeCfg)

	wssTarget := ResolveProbeTarget(probeCfg, "104.16.1.1", 8443)
	if wssTarget.SNI != "vps.example.com" || wssTarget.Host != "vps.example.com" || wssTarget.Path != "/pyway" || wssTarget.Port != 8443 {
		t.Fatalf("expected WSS target to use VPS params, got %+v", wssTarget)
	}

	speedTarget := ResolveSpeedTargetForProbe(probeCfg, "104.16.1.1", 8443)
	if speedTarget.ProfileType != ProfileCFST || speedTarget.SNI != "speed.cloudflare.com" || speedTarget.Host != "speed.cloudflare.com" || !strings.Contains(speedTarget.URL, "speed.cloudflare.com/__down") {
		t.Fatalf("expected GOWAY speed target to route to speed.cloudflare.com, got %+v", speedTarget)
	}

	// Custom HTTPS URL must stay on the custom URL/Host/SNI
	customCfg := DefaultProbeConfig()
	customCfg.Profile = NewProfileCustom("https://cdn.example.com:8443/big.bin", "cdn.example.com", 8443)
	NormalizeProbeConfig(&customCfg)
	customSpeedTarget := ResolveSpeedTargetForProbe(customCfg, "104.16.1.1", 8443)
	if customSpeedTarget.Host != "cdn.example.com:8443" || customSpeedTarget.SNI != "cdn.example.com" || customSpeedTarget.URL != "https://cdn.example.com:8443/big.bin" {
		t.Fatalf("expected Custom HTTPS speed target to preserve custom host/URL, got %+v", customSpeedTarget)
	}
}

func TestFromNodeResultPreservesGOWAYWSS(t *testing.T) {
	in := NodeResult{
		IP:                   "172.64.10.20",
		Port:                 443,
		TCPLatency:           42.5,
		Jitter:               2.1,
		PacketLoss:           0,
		Colo:                 "NRT",
		DownloadSpeed:        48.0,
		SingleSpeed:          48.0,
		P10Speed:             39.0,
		MinSpeed:             35.0,
		Stability:            92.0,
		GOWAYWSSCompatible:   true,
		GOWAYWSSLatency:      88.4,
		WSSJitter:            3.6,
		WSSSuccessRate:       1.0,
		GOWAYWSSErrorStage:   "",
		GOWAYWSSHTTPStatus:   101,
		GOWAYWSSErrorMessage: "",
		GOWAYWSSSNISent:      "vps.example.com",
		GOWAYWSSHostSent:     "vps.example.com",
		GOWAYWSSPathSent:     "/pyway",
	}

	rm := FromNodeResult(in, TierCandidate)
	if !rm.GOWAYWSSCompatible || rm.GOWAYWSSLatency != 88.4 || rm.WSSJitter != 3.6 || rm.WSSSuccessRate != 1.0 || rm.GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("FromNodeResult dropped GOWAY WSS metrics: %+v", rm)
	}
	if rm.GOWAYWSSSNISent != "vps.example.com" || rm.GOWAYWSSHostSent != "vps.example.com" || rm.GOWAYWSSPathSent != "/pyway" {
		t.Fatalf("FromNodeResult dropped GOWAY WSS routing metadata: %+v", rm)
	}

	roundTrip := rm.ToNodeResult()
	if !roundTrip.GOWAYWSSCompatible || roundTrip.GOWAYWSSLatency != 88.4 || roundTrip.WSSJitter != 3.6 || roundTrip.WSSSuccessRate != 1.0 {
		t.Fatalf("ToNodeResult dropped GOWAY WSS metrics: %+v", roundTrip)
	}
}

func TestProbeScheduler_CandidateCalibrationAndPriority(t *testing.T) {
	store := NewRouteStore()
	cfg := DefaultProbeConfig()
	cfg.MaxConcurrent = 1 // force sequential priority queue selection per tick
	cfg.ActiveInterval = 10 * time.Second
	cfg.CandidateInterval = 10 * time.Second
	ps := NewProbeScheduler(store, cfg)

	now := time.Now()
	// Insert 10 overdue Candidate routes first, then 1 overdue Active route
	for i := 1; i <= 10; i++ {
		store.UpsertRoute(RouteMetrics{
			ID:         GenerateRouteID(fmt.Sprintf("10.0.0.%d", i), 443),
			IP:         fmt.Sprintf("10.0.0.%d", i),
			Port:       443,
			Tier:       TierCandidate,
			LastTested: now.Add(-60 * time.Second),
		})
	}
	store.UpsertRoute(RouteMetrics{
		ID:         GenerateRouteID("10.0.1.1", 443),
		IP:         "10.0.1.1",
		Port:       443,
		Tier:       TierActive,
		LastTested: now.Add(-20 * time.Second),
	})

	var mu sync.Mutex
	var firstDispatched string
	dispatchedCh := make(chan string, 20)
	ps.probeExec = func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
		mu.Lock()
		if firstDispatched == "" {
			firstDispatched = target.IP
		}
		mu.Unlock()
		dispatchedCh <- target.IP
		return LayeredProbeResult{Success: true, RTT: 30, HandshakeSuccess: true, DownloadSpeed: 20, SingleSpeed: 20, P10Speed: 15, Stability: 85}
	}

	ps.evaluateAndSchedule(context.Background(), now)
	select {
	case <-dispatchedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled probe")
	}
	if firstDispatched != "10.0.1.1" {
		t.Fatalf("expected overdue TierActive route 10.0.1.1 to be dispatched first, got %s", firstDispatched)
	}

	// Verify TierCandidate missing P10Speed triggers L4 calibration (runFullSpeed=true) on cycle 2
	var candFullSpeed []bool
	candIP := "10.0.2.5"
	store.UpsertRoute(RouteMetrics{
		ID:            GenerateRouteID(candIP, 443),
		IP:            candIP,
		Port:          443,
		Tier:          TierCandidate,
		DownloadSpeed: 18,
		SingleSpeed:   18,
		P10Speed:      0,
	})
	ps.probeExec = func(ctx context.Context, target ResolvedProbeTarget, cfg ProbeConfig, runL3 bool, runFullSpeed bool) LayeredProbeResult {
		candFullSpeed = append(candFullSpeed, runFullSpeed)
		return LayeredProbeResult{
			Success:          true,
			RTT:              25,
			HandshakeSuccess: true,
			DownloadSpeed:    30,
			SingleSpeed:      30,
			P10Speed:         0, // keep 0 until verified after cycle 2
		}
	}
	if _, err := ps.ProbeOnce(context.Background(), candIP, false); err != nil {
		t.Fatalf("cycle 1 error: %v", err)
	}
	if _, err := ps.ProbeOnce(context.Background(), candIP, false); err != nil {
		t.Fatalf("cycle 2 error: %v", err)
	}
	if len(candFullSpeed) != 2 || candFullSpeed[0] || !candFullSpeed[1] {
		t.Fatalf("expected cycle 1 runFullSpeed=false and cycle 2 runFullSpeed=true for Candidate without P10, got %+v", candFullSpeed)
	}
}

func TestRunQuickFilter_NoDeadNodeBackfill(t *testing.T) {
	// Healthy server streaming payload over ~300ms (exceeding SingleStreamTestDetailed 100ms floor)
	srvOK := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		payload := bytes.Repeat([]byte("A"), 128*1024)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			_, _ = w.Write(payload)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srvOK.Close()

	// Dead/403 server returning 403 Forbidden
	srvDead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srvDead.Close()

	_, portOKStr, _ := net.SplitHostPort(srvOK.Listener.Addr().String())
	portOK, _ := strconv.Atoi(portOKStr)
	_, portDeadStr, _ := net.SplitHostPort(srvDead.Listener.Addr().String())
	portDead, _ := strconv.Atoi(portDeadStr)

	// Test healthy candidate alone vs 403 candidates using distinct local ports via ResolveProbeTarget behavior:
	// First verify srvOK yields speed > 0 and srvDead yields no backfill in runQuickFilter
	cfgDead := DefaultConfig()
	cfgDead.Profile = "CUSTOM"
	cfgDead.Port = portDead
	cfgDead.URL = srvDead.URL + "/test"
	cfgDead.SNI = "localhost"
	cfgDead.QuickDuration = 2
	cfgDead.DLConc = 2

	deadCands := []NodeResult{
		{IP: "127.0.0.1", Port: portDead, TCPLatency: 5},
		{IP: "127.0.0.1", Port: portDead, TCPLatency: 6},
	}
	gotDead := runQuickFilter(context.Background(), deadCands, cfgDead, 5, nil)
	if len(gotDead) != 0 {
		t.Fatalf("expected 0 nodes when all candidates are dead/403 (no backfill), got %d", len(gotDead))
	}

	cfgOK := DefaultConfig()
	cfgOK.Profile = "CUSTOM"
	cfgOK.Port = portOK
	cfgOK.URL = srvOK.URL + "/test"
	cfgOK.SNI = "localhost"
	cfgOK.QuickDuration = 2
	cfgOK.DLConc = 1
	gotOK := runQuickFilter(context.Background(), []NodeResult{{IP: "127.0.0.1", Port: portOK, TCPLatency: 5}}, cfgOK, 5, nil)
	if len(gotOK) != 1 || gotOK[0].DownloadSpeed <= 0 {
		t.Fatalf("expected 1 healthy node with speed > 0, got %+v", gotOK)
	}
}

func TestMeasureWSSStability_Local101Mock(t *testing.T) {
	tlsConf := selfSignedTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\ncf-ray: abc123-NRT\r\n\r\n"))
			}(conn)
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	avg, jit, rate, colo := MeasureWSSStability(context.Background(), "127.0.0.1", port, "vps.example.com", "vps.example.com", "/pyway", 3, 2*time.Second)
	if rate != 1.0 || avg <= 0 || jit < 0 || colo != "NRT" {
		t.Fatalf("unexpected MeasureWSSStability result: avg=%.2f jit=%.2f rate=%.2f colo=%q", avg, jit, rate, colo)
	}
}

func TestCalcScore_WSSSuccessRateAndPacketLossGate(t *testing.T) {
	// Node A: high speed but 33% WSS handshake success rate
	flakyWSS := NodeResult{
		IP:                 "172.64.151.138",
		Colo:               "SIN",
		TCPLatency:         110.0,
		Jitter:             14.8,
		PacketLoss:         0.0,
		DownloadSpeed:      28.62,
		SingleSpeed:        28.62,
		P10Speed:           19.22,
		MinSpeed:           16.78,
		LoadLatency:        141.2,
		Stability:          79.0,
		GOWAYWSSCompatible: true,
		GOWAYWSSLatency:    1831.3,
		WSSSuccessRate:     0.33,
	}
	flakyWSS.CalcScore()

	// Node B: 100% WSS handshake success rate + higher P10/stability, even with 20% rechecked loss
	stableWSS20Loss := NodeResult{
		IP:                 "172.64.156.150",
		Colo:               "SIN",
		TCPLatency:         130.0,
		Jitter:             0.7,
		PacketLoss:         0.20,
		DownloadSpeed:      29.46,
		SingleSpeed:        29.46,
		P10Speed:           25.52,
		MinSpeed:           21.61,
		LoadLatency:        157.5,
		Stability:          87.0,
		GOWAYWSSCompatible: true,
		GOWAYWSSLatency:    1798.2,
		WSSSuccessRate:     1.0,
	}
	stableWSS20Loss.CalcScore()

	if stableWSS20Loss.Score <= flakyWSS.Score {
		t.Fatalf("expected 100%% WSS node (%.1f) to outscore 33%% WSS node (%.1f)", stableWSS20Loss.Score, flakyWSS.Score)
	}

	// ScoreEngine.EvaluateRoute must also apply the WSS handshake instability penalty
	engine := NewScoreEngine(ModeNormal)
	rm := FromNodeResult(flakyWSS, TierCandidate)
	engine.EvaluateRoute(rm, EWMASnapshot{}, EWMASnapshot{}, 0)
	if rm.FinalScore >= 75.0 {
		t.Fatalf("expected EvaluateRoute FinalScore to reflect 33%% WSS penalty (<75), got %.1f", rm.FinalScore)
	}

	// EvaluateTierTransitions must block 33% WSS route from Standby/Active promotion
	origSchedCfg := GlobalProbeScheduler.GetConfig()
	defer GlobalProbeScheduler.UpdateConfig(origSchedCfg)
	wssSchedCfg := origSchedCfg
	wssSchedCfg.Profile = NewProfileGOWAYWSS("vps.example.com", "/pyway", "vps.example.com", 443)
	GlobalProbeScheduler.UpdateConfig(wssSchedCfg)

	store := NewRouteStore()
	rm.ObservationDuration = 600
	rm.Confidence = 70
	rm.FinalScore = 80
	store.UpsertRoute(*rm)
	store.mu.Lock()
	store.routes[rm.ID].Samples = make([]MeasurementSample, 5)
	store.mu.Unlock()
	store.EvaluateTierTransitions(time.Now())
	tierAfter, _ := store.GetMetrics(rm.IP)
	if tierAfter.Tier != TierCandidate {
		t.Fatalf("expected 33%% WSS route to stay TierCandidate, got %s", tierAfter.Tier)
	}
}

func TestRoutePreRankKey_AndSaveCSVColumns(t *testing.T) {
	a := NodeResult{TCPLatency: 100, Jitter: 2, PacketLoss: 0, GOWAYWSSLatency: 1200}
	b := NodeResult{TCPLatency: 100, Jitter: 2, PacketLoss: 0, GOWAYWSSLatency: 400}
	if routePreRankKey(b) >= routePreRankKey(a) {
		t.Fatalf("expected lower WSSLatency node to pre-rank better: a=%.1f b=%.1f", routePreRankKey(a), routePreRankKey(b))
	}
	if a.TCPLatency != 100 || b.TCPLatency != 100 {
		t.Fatalf("routePreRankKey must not mutate TCPLatency")
	}

	csvPath := filepath.Join(t.TempDir(), "out.csv")
	saveCSV(csvPath, []NodeResult{{
		IP:              "172.64.156.150",
		Colo:            "SIN",
		TCPLatency:      120.5,
		Jitter:          0.7,
		PacketLoss:      0.2,
		SingleSpeed:     29.46,
		DownloadSpeed:   29.46,
		P10Speed:        25.52,
		MinSpeed:        21.61,
		LoadLatency:     157.5,
		Stability:       87,
		StallCount:      1,
		GOWAYWSSLatency: 1798.2,
		WSSJitter:       28.5,
		WSSSuccessRate:  1.0,
		Score:           77.8,
	}})
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatalf("open csv: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("read csv failed: rows=%d err=%v", len(rows), err)
	}
	header := strings.Join(rows[0], ",")
	if !strings.Contains(header, "Loss") || !strings.Contains(header, "Stalls") {
		t.Fatalf("expected Loss and Stalls columns in CSV header, got %q", header)
	}
	if rows[1][5] != "20%" || rows[1][12] != "1" {
		t.Fatalf("unexpected Loss/Stalls cell values in CSV row: %+v", rows[1])
	}
}

func TestMeasureTCPPingStats_LocalListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	avg, jit, loss, ok := measureTCPPingStats(context.Background(), "127.0.0.1", port, 5, 1, 0, 500*time.Millisecond)
	_ = ln.Close()
	if !ok || loss != 0 || avg <= 0 || jit < 0 {
		t.Fatalf("expected clean local TCP ping stats, got avg=%.2f jit=%.2f loss=%.2f ok=%v", avg, jit, loss, ok)
	}

	_, _, closedLoss, closedOK := measureTCPPingStats(context.Background(), "127.0.0.1", port, 3, 2, 2, 200*time.Millisecond)
	if closedOK || closedLoss != 1.0 {
		t.Fatalf("expected closed port to fail with loss=1.0, got ok=%v loss=%.2f", closedOK, closedLoss)
	}
}

func TestSelectDiverseCandidates_RejectsDistantAndLossyBuckets(t *testing.T) {
	var nodes []NodeResult
	// 12 fast SIN nodes in 172.64.144.0/20 (95~106ms, 0% loss)
	for i := 0; i < 12; i++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("172.64.%d.10", 144+i),
			Colo:       "SIN",
			TCPLatency: 95.0 + float64(i),
			Jitter:     1.2,
			PacketLoss: 0.0,
		})
	}
	// 8 fast HKG nodes in 162.159.192.0/20 (105~112ms, 0% loss)
	for i := 0; i < 8; i++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("162.159.%d.20", 192+i),
			Colo:       "HKG",
			TCPLatency: 105.0 + float64(i),
			Jitter:     1.5,
			PacketLoss: 0.0,
		})
	}
	// 120 distant FRA/LAX/AMS buckets (each in its own /20!) with high latency (340~500ms) or 25% loss
	for b := 0; b < 120; b++ {
		nodes = append(nodes, NodeResult{
			IP:         fmt.Sprintf("104.%d.%d.50", 16+(b/16), (b%16)*16+1),
			Colo:       "FRA",
			TCPLatency: 340.0 + float64(b),
			Jitter:     15.0,
			PacketLoss: 0.25,
		})
	}

	selected := selectDiverseCandidates(nodes, 20)
	if len(selected) != 20 {
		t.Fatalf("expected 20 selected candidates, got %d", len(selected))
	}
	for _, s := range selected {
		if s.Colo == "FRA" || s.TCPLatency > 200 || s.PacketLoss > 0 {
			t.Fatalf("distant/lossy bucket leaked into top 20 elite pool: %+v", s)
		}
	}
}

func TestPiecewiseLatencyScoreAndInterleavedSubnets(t *testing.T) {
	eng := NewScoreEngine(ModeNormal)
	w := eng.ActiveWeights()
	// 210ms node must score strictly higher than 420ms node (was both 0 on latency in v2.5.1)
	s210 := eng.calcMetricScore(w, 25, 20, 15, 210.0, 1.0, 0.0, 90.0, true, "SIN")
	s420 := eng.calcMetricScore(w, 25, 20, 15, 420.0, 1.0, 0.0, 90.0, true, "SIN")
	if s210 <= s420 {
		t.Fatalf("expected 210ms node (%.1f) to score higher than 420ms node (%.1f)", s210, s420)
	}

	// Standard 1,500 sample must cover >= 150 distinct /20 prefixes
	ips := GenerateIPs(1500, true, "")
	if len(ips) != 1500 {
		t.Fatalf("expected 1500 IPs, got %d", len(ips))
	}
	seen20 := make(map[string]bool)
	for _, ip := range ips {
		seen20[prefix20Key(ip)] = true
	}
	if len(seen20) < 150 {
		t.Fatalf("expected interleaved /24 sampling to span >= 150 distinct /20 blocks, got %d", len(seen20))
	}
}

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
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

type wssMockServer struct {
	ln                net.Listener
	port              int
	mu                sync.Mutex
	requiredSNI       string
	requiredHost      string
	requiredPath      string
	statusCode        int
	responseBody      string
	tcpPingCount      int
	tlsHandshakeCount int
	gotSNI            string
	gotHost           string
	gotPath           string
	gotUpgrade        string
	conns             atomic.Int32
	cert              tls.Certificate
}

func generateTestCertificate(t *testing.T) tls.Certificate {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: "127.0.0.1",
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
}

func startWSSMockServer(t *testing.T, statusCode int, responseBody string) *wssMockServer {
	cert := generateTestCertificate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	s := &wssMockServer{
		ln:           ln,
		port:         ln.Addr().(*net.TCPAddr).Port,
		statusCode:   statusCode,
		responseBody: responseBody,
		cert:         cert,
	}

	go func() {
		for {
			rawConn, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go s.serveConn(rawConn)
		}
	}()

	// Readiness check: fast dial to ensure listener is accepting
	for i := 0; i < 30; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	ln.Close()
	t.Fatalf("wssMockServer on 127.0.0.1:%d failed to become ready", s.port)
	return nil
}

func (s *wssMockServer) serveConn(rawConn net.Conn) {
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(3 * time.Second))

	var firstByte [1]byte
	n, err := rawConn.Read(firstByte[:])
	if err != nil || n == 0 {
		// Pure TCP Ping: client connected and closed without writing any bytes
		s.mu.Lock()
		s.tcpPingCount++
		s.mu.Unlock()
		return
	}

	if firstByte[0] != 0x16 {
		// Not TLS ClientHello
		return
	}

	serverTLSConfig := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.mu.Lock()
			reqSNI := s.requiredSNI
			s.mu.Unlock()
			if reqSNI != "" && hello.ServerName != reqSNI {
				return nil, fmt.Errorf("SNI mismatch: expected %s, got %s", reqSNI, hello.ServerName)
			}
			return &tls.Config{
				Certificates: []tls.Certificate{s.cert},
			}, nil
		},
	}

	wrapped := &bufferedConn{
		Conn: rawConn,
		r:    io.MultiReader(bytes.NewReader(firstByte[:]), rawConn),
	}
	tlsConn := tls.Server(wrapped, serverTLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return
	}

	s.mu.Lock()
	s.tlsHandshakeCount++
	s.gotSNI = tlsConn.ConnectionState().ServerName
	s.mu.Unlock()

	br := bufio.NewReader(tlsConn)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}

	reqParts := strings.Split(strings.TrimSpace(reqLine), " ")
	reqPath := "/"
	if len(reqParts) >= 2 {
		reqPath = reqParts[1]
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

	s.mu.Lock()
	if upgrade != "" {
		s.gotUpgrade = upgrade
		s.gotHost = host
		s.gotPath = reqPath
	} else if s.gotUpgrade == "" {
		s.gotHost = host
		s.gotPath = reqPath
		s.gotUpgrade = upgrade
	}
	status := s.statusCode
	body := s.responseBody
	reqHost := s.requiredHost
	reqExpectedPath := s.requiredPath
	s.mu.Unlock()

	if reqHost != "" && host != reqHost {
		status = http.StatusForbidden
		body = "Host forbidden"
	} else if reqExpectedPath != "" && reqPath != reqExpectedPath {
		status = http.StatusNotFound
		body = "Path not found"
	}

	if status == 101 {
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"
		_, _ = tlsConn.Write([]byte(resp))
	} else {
		resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			status, http.StatusText(status), len(body), body)
		_, _ = tlsConn.Write([]byte(resp))
	}
}

func (s *wssMockServer) snapshot() (tcpCount, tlsCount int, sni, host, path, upgrade string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpPingCount, s.tlsHandshakeCount, s.gotSNI, s.gotHost, s.gotPath, s.gotUpgrade
}

func (s *wssMockServer) close() {
	s.ln.Close()
}

// 1. TestWSSHTTP101Success: Returns HTTP/1.1 101 Switching Protocols -> Success == true, HTTPStatus == 101, ErrorStage == "".
func TestWSSHTTP101Success(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", "/pyway", 3*time.Second)
	if !res.Success {
		t.Fatalf("expected Success=true, got %+v", res)
	}
	if res.ErrorStage != "" {
		t.Fatalf("expected empty ErrorStage, got %q", res.ErrorStage)
	}
	if res.HTTPStatus != 101 {
		t.Fatalf("expected HTTPStatus=101, got %d", res.HTTPStatus)
	}
	if !res.TCPSuccess || !res.TLSHandshake {
		t.Fatalf("expected TCPSuccess and TLSHandshake to be true, got %+v", res)
	}
	if res.Latency <= 0 {
		t.Fatalf("expected positive Latency, got %f", res.Latency)
	}
}

// 2. TestWSSHTTP200Rejected: Returns HTTP/1.1 200 OK -> Success == false, HTTPStatus == 200, ErrorStage == "status".
func TestWSSHTTP200Rejected(t *testing.T) {
	srv := startWSSMockServer(t, 200, "OK")
	defer srv.close()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", "/pyway", 3*time.Second)
	if res.Success {
		t.Fatalf("expected Success=false for HTTP 200, got %+v", res)
	}
	if res.HTTPStatus != 200 {
		t.Fatalf("expected HTTPStatus=200, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "status" {
		t.Fatalf("expected ErrorStage='status', got %q", res.ErrorStage)
	}
}

// 3. TestWSSBodyContains101StillRejected: Returns HTTP/1.1 403 Forbidden with body containing "error code: 101" -> Success == false, HTTPStatus == 403, ErrorStage == "status".
func TestWSSBodyContains101StillRejected(t *testing.T) {
	srv := startWSSMockServer(t, 403, "error code: 101")
	defer srv.close()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", "/pyway", 3*time.Second)
	if res.Success {
		t.Fatalf("expected Success=false even when body contains '101', got %+v", res)
	}
	if res.HTTPStatus != 403 {
		t.Fatalf("expected HTTPStatus=403, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "status" {
		t.Fatalf("expected ErrorStage='status', got %q", res.ErrorStage)
	}
}

// 4. TestWSSHostAndSNIIndependent: SNI and Host sent on wire must match independently with zero mutual fallbacks.
func TestWSSHostAndSNIIndependent(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()
	srv.mu.Lock()
	srv.requiredSNI = "sni.example.com"
	srv.requiredHost = "host.example.com"
	srv.mu.Unlock()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", "/pyway", 3*time.Second)
	if !res.Success {
		t.Fatalf("expected Success=true with independent SNI and Host, got %+v", res)
	}
	_, _, gotSNI, gotHost, _, _ := srv.snapshot()
	if gotSNI != "sni.example.com" {
		t.Fatalf("expected SNI 'sni.example.com', got %q", gotSNI)
	}
	if gotHost != "host.example.com" {
		t.Fatalf("expected Host 'host.example.com', got %q", gotHost)
	}
	if res.SNISent != "sni.example.com" {
		t.Fatalf("expected SNISent 'sni.example.com', got %q", res.SNISent)
	}
	if res.HostSent != "host.example.com" {
		t.Fatalf("expected HostSent 'host.example.com', got %q", res.HostSent)
	}
}

// 5. TestWSSCustomPath: Custom paths (/pyway, /custom-ws, /test) must be correctly sent on wire and matched by listener.
func TestWSSCustomPath(t *testing.T) {
	paths := []string{"/pyway", "/custom-ws", "/test"}
	for _, p := range paths {
		srv := startWSSMockServer(t, 101, "")
		srv.mu.Lock()
		srv.requiredPath = p
		srv.mu.Unlock()

		res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", p, 3*time.Second)
		_, _, _, _, gotPath, _ := srv.snapshot()
		srv.close()

		if !res.Success {
			t.Fatalf("path %s: expected Success=true, got %+v", p, res)
		}
		if gotPath != p {
			t.Fatalf("path %s: expected server gotPath %s, got %s", p, p, gotPath)
		}
		if res.PathSent != p {
			t.Fatalf("path %s: expected PathSent %s, got %s", p, p, res.PathSent)
		}
	}
}

// 6. TestWSSWrongHostRejected: When server expects correct.example.com but client sends wrong.example.com -> HTTP 403, Success == false.
func TestWSSWrongHostRejected(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()
	srv.mu.Lock()
	srv.requiredHost = "correct.example.com"
	srv.mu.Unlock()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "wrong.example.com", "/pyway", 3*time.Second)
	if res.Success {
		t.Fatalf("expected Success=false for wrong host, got %+v", res)
	}
	if res.HTTPStatus != 403 {
		t.Fatalf("expected HTTPStatus=403, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "status" {
		t.Fatalf("expected ErrorStage='status', got %q", res.ErrorStage)
	}
}

// 7. TestWSSWrongSNIRejected: When server expects correct-sni.example.com but client sends wrong-sni.example.com -> TLS handshake failure.
func TestWSSWrongSNIRejected(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()
	srv.mu.Lock()
	srv.requiredSNI = "correct-sni.example.com"
	srv.mu.Unlock()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "wrong-sni.example.com", "host.example.com", "/pyway", 3*time.Second)
	if res.Success {
		t.Fatalf("expected Success=false for wrong SNI, got %+v", res)
	}
	if res.ErrorStage != "tls" {
		t.Fatalf("expected ErrorStage='tls', got %q", res.ErrorStage)
	}
}

// 8. TestWSSWrongPathRejected: When server expects /correct-path but client sends /wrong-path -> HTTP 404, Success == false.
func TestWSSWrongPathRejected(t *testing.T) {
	srv := startWSSMockServer(t, 101, "")
	defer srv.close()
	srv.mu.Lock()
	srv.requiredPath = "/correct-path"
	srv.mu.Unlock()

	res := WSSHandshakeCheckDetailed("127.0.0.1", srv.port, "sni.example.com", "host.example.com", "/wrong-path", 3*time.Second)
	if res.Success {
		t.Fatalf("expected Success=false for wrong path, got %+v", res)
	}
	if res.HTTPStatus != 404 {
		t.Fatalf("expected HTTPStatus=404, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "status" {
		t.Fatalf("expected ErrorStage='status', got %q", res.ErrorStage)
	}
}

// 9. TestWSSConfigIncomplete: Incomplete configs must return ErrorStage="config" without dialing any network connection.
func TestWSSConfigIncomplete(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		port int
		sni  string
		host string
		path string
	}{
		{"empty_host", "127.0.0.1", 443, "sni.example.com", "", "/pyway"},
		{"empty_sni", "127.0.0.1", 443, "", "host.example.com", "/pyway"},
		{"empty_path", "127.0.0.1", 443, "sni.example.com", "host.example.com", ""},
		{"zero_port", "127.0.0.1", 0, "sni.example.com", "host.example.com", "/pyway"},
		{"negative_port", "127.0.0.1", -1, "sni.example.com", "host.example.com", "/pyway"},
		{"empty_ip", "", 443, "sni.example.com", "host.example.com", "/pyway"},
	}

	srv := startWSSMockServer(t, 101, "")
	defer srv.close()

	for _, tc := range cases {
		port := tc.port
		if port == 443 {
			port = srv.port
		}
		res := WSSHandshakeCheckDetailed(tc.ip, port, tc.sni, tc.host, tc.path, 1*time.Second)
		if res.Success {
			t.Fatalf("%s: expected Success=false, got %+v", tc.name, res)
		}
		if res.ErrorStage != "config" {
			t.Fatalf("%s: expected ErrorStage='config', got %q", tc.name, res.ErrorStage)
		}
		if res.ErrorMessage == "" {
			t.Fatalf("%s: expected non-empty ErrorMessage", tc.name)
		}
	}

	// Verify 0 connections were accepted on srv (excluding initial readiness check dial)
	if srv.conns.Load() > 1 {
		t.Fatalf("expected 0 dials for incomplete config, but server accepted %d connections", srv.conns.Load())
	}
}

// 10. TestCFSTDefaultDoesNotUseWSS: ProfileCFST must use speed.cloudflare.com HTTPS and never issue WSS upgrade.
func TestCFSTDefaultDoesNotUseWSS(t *testing.T) {
	defCfg := DefaultConfig()
	prof := defCfg.GetProbeProfile()
	if prof.Type != ProfileCFST {
		t.Fatalf("expected DefaultConfig profile to be ProfileCFST, got %s", prof.Type)
	}
	if prof.Protocol != "https" {
		t.Fatalf("expected DefaultConfig protocol to be https, got %s", prof.Protocol)
	}
	if !strings.Contains(prof.TestURL, "speed.cloudflare.com") {
		t.Fatalf("expected CFST test URL speed.cloudflare.com, got %s", prof.TestURL)
	}

	srv := startWSSMockServer(t, 101, "")
	defer srv.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, srv.port, 1, prof, nil)

	_, _, _, _, _, gotUpgrade := srv.snapshot()
	if strings.ToLower(gotUpgrade) == "websocket" {
		t.Fatalf("ProfileCFST unexpectedly sent a WebSocket upgrade request!")
	}
}

// 11. TestGOWAYScanGateHTTPSGoodWSSBad: GOWAY candidate gating admits node on HTTP 101, rejects on HTTP 200.
func TestGOWAYScanGateHTTPSGoodWSSBad(t *testing.T) {
	// Case A: WSS returns 101 -> Node admitted with GOWAYWSSCompatible = true and diagnostics
	srvOK := startWSSMockServer(t, 101, "")
	profOK := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", srvOK.port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	nodesOK, _, _ := ScanRoutesWithProfileDetailed(ctx, []string{"127.0.0.1"}, srvOK.port, 1, profOK, nil)
	cancel()
	srvOK.close()

	if len(nodesOK) != 1 {
		t.Fatalf("Case A: expected 1 valid node, got %d", len(nodesOK))
	}
	if !nodesOK[0].GOWAYWSSCompatible {
		t.Fatalf("Case A: expected GOWAYWSSCompatible=true, got %+v", nodesOK[0])
	}
	if nodesOK[0].GOWAYWSSHTTPStatus != 101 {
		t.Fatalf("Case A: expected GOWAYWSSHTTPStatus=101, got %d", nodesOK[0].GOWAYWSSHTTPStatus)
	}
	if nodesOK[0].GOWAYWSSLatency <= 0 {
		t.Fatalf("Case A: expected positive latency, got %f", nodesOK[0].GOWAYWSSLatency)
	}

	// Case B: WSS returns 200 -> Node rejected
	srvBad := startWSSMockServer(t, 200, "OK")
	profBad := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", srvBad.port)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	nodesBad, _, _ := ScanRoutesWithProfileDetailed(ctx2, []string{"127.0.0.1"}, srvBad.port, 1, profBad, nil)
	cancel2()
	srvBad.close()

	if len(nodesBad) != 0 {
		t.Fatalf("Case B: expected 0 nodes admitted when WSS returns 200, got %d", len(nodesBad))
	}
}

// 12. TestGOWAYProfileFullyDrivenByConfig: Host, SNI, Path, Port flow cleanly from Config into ProbeProfile.
func TestGOWAYProfileFullyDrivenByConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profile = "GOWAY-WSS"
	cfg.WSSHost = "future.example.org"
	cfg.WSSPath = "/future-ws"
	cfg.SNI = "sni.future.example.org"
	cfg.Port = 8443

	prof := cfg.GetProbeProfile()
	if prof.Type != ProfileGOWAYWSS {
		t.Fatalf("expected GOWAY-WSS profile, got %s", prof.Type)
	}
	if prof.Host != "future.example.org" || prof.SNI != "sni.future.example.org" || prof.Path != "/future-ws" || prof.Port != 8443 {
		t.Fatalf("profile not fully driven by config: %+v", prof)
	}

	probeCfg := ConfigToProbeConfig(cfg)
	if probeCfg.Profile.Host != "future.example.org" || probeCfg.Profile.SNI != "sni.future.example.org" || probeCfg.Profile.Path != "/future-ws" || probeCfg.Profile.Port != 8443 {
		t.Fatalf("daemon probe config not fully driven by config: %+v", probeCfg.Profile)
	}
	if probeCfg.Profile.Protocol != "wss" {
		t.Fatalf("expected wss protocol, got %s", probeCfg.Profile.Protocol)
	}
}

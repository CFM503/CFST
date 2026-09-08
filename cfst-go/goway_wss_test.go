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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type testPrefixConn struct {
	net.Conn
	r io.Reader
}

func (c *testPrefixConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

func generateTestTLSConfig(t *testing.T) *tls.Config {
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
		DNSNames:     []string{"localhost", "edge-test.example.com", "sni-test.example.com", "another.example.net", "sni2.example.net", "edge.goway.custom"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create test certificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
	}
}

// startDualProtocolListener starts a test server that distinguishes:
// 1. Plain TCP ping (immediate close, no bytes sent) -> quickly closes conn with short deadline
// 2. TLS connection (first byte 0x16) -> performs TLS handshake and delegates to onTLS
// 3. Plain HTTP connection -> delegates to onHTTP
func startDualProtocolListener(t *testing.T, tlsConfig *tls.Config,
	onTLS func(tlsConn *tls.Conn, sni, host, path, upgrade string) (statusCode int, body string),
	onHTTP func(conn net.Conn, reqLine string, headers map[string]string)) (net.Listener, int) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
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
				// Step 1: Detect plain TCP ping (sends 0 bytes and closes)
				_ = c.SetDeadline(time.Now().Add(150 * time.Millisecond))
				var first [1]byte
				n, err := c.Read(first[:])
				if err != nil || n == 0 {
					// Plain TCP ping: immediate clean close
					return
				}

				// Received application data: reset to standard deadline
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				wrapped := &testPrefixConn{
					Conn: c,
					r:    io.MultiReader(bytes.NewReader(first[:]), c),
				}

				if first[0] == 0x16 {
					// TLS connection (WSS or HTTPS)
					tlsConn := tls.Server(wrapped, tlsConfig)
					if err := tlsConn.Handshake(); err != nil {
						return
					}
					defer tlsConn.Close()

					br := bufio.NewReader(tlsConn)
					reqLine, err := br.ReadString('\n')
					if err != nil {
						return
					}
					parts := strings.Split(strings.TrimSpace(reqLine), " ")
					reqPath := "/"
					if len(parts) >= 2 {
						reqPath = parts[1]
					}

					host := ""
					upgrade := ""
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						line = strings.TrimSpace(line)
						if line == "" {
							break
						}
						if idx := strings.Index(line, ":"); idx > 0 {
							k := strings.ToLower(strings.TrimSpace(line[:idx]))
							v := strings.TrimSpace(line[idx+1:])
							switch k {
							case "host":
								host = v
							case "upgrade":
								upgrade = v
							}
						}
					}

					sni := tlsConn.ConnectionState().ServerName
					status, body := onTLS(tlsConn, sni, host, reqPath, upgrade)
					if status == 101 {
						_, _ = tlsConn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
					} else {
						_, _ = fmt.Fprintf(tlsConn, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
							status, http.StatusText(status), len(body), body)
					}
					return
				}

				// Plain HTTP connection
				br := bufio.NewReader(wrapped)
				reqLine, err := br.ReadString('\n')
				if err != nil {
					return
				}
				headers := make(map[string]string)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if line == "" {
						break
					}
					if idx := strings.Index(line, ":"); idx > 0 {
						k := strings.ToLower(strings.TrimSpace(line[:idx]))
						v := strings.TrimSpace(line[idx+1:])
						headers[k] = v
					}
				}
				if onHTTP != nil {
					onHTTP(c, strings.TrimSpace(reqLine), headers)
				}
			}(conn)
		}
	}()

	// Readiness pre-flight
	for i := 0; i < 20; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ln, port
}

// TestWSS_CaseA_HTTP101Accepted verifies Case A: TCP OK + TLS OK + HTTP 101 -> candidate accepted.
func TestWSS_CaseA_HTTP101Accepted(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)
	var capturedHost, capturedSNI, capturedPath string
	var mu sync.Mutex

	ln, port := startDualProtocolListener(t, tlsConf,
		func(tlsConn *tls.Conn, sni, host, path, upgrade string) (int, string) {
			mu.Lock()
			capturedSNI = sni
			capturedHost = host
			capturedPath = path
			mu.Unlock()
			return 101, ""
		}, nil)
	defer ln.Close()

	prof := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes := ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, port, 1, prof, nil)
	if len(nodes) != 1 {
		t.Fatalf("Case A: expected candidate accepted (1 node), got %d nodes", len(nodes))
	}

	mu.Lock()
	gotSNI, gotHost, gotPath := capturedSNI, capturedHost, capturedPath
	mu.Unlock()

	if gotHost != "edge-test.example.com" {
		t.Fatalf("expected Host edge-test.example.com, got %s", gotHost)
	}
	if gotSNI != "sni-test.example.com" {
		t.Fatalf("expected SNI sni-test.example.com, got %s", gotSNI)
	}
	if gotPath != "/test-one" {
		t.Fatalf("expected Path /test-one, got %s", gotPath)
	}
}

// TestWSS_CaseB_HTTP200Rejected verifies Case B: TCP OK + TLS OK + HTTP 200 (even with body containing 101) -> candidate rejected.
func TestWSS_CaseB_HTTP200Rejected(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)

	ln, port := startDualProtocolListener(t, tlsConf,
		func(tlsConn *tls.Conn, sni, host, path, upgrade string) (int, string) {
			// Body maliciously contains "101 Switching Protocols" to test strict parsing
			return 200, "{\"code\": 101, \"msg\": \"101 Switching Protocols in body\"}"
		}, nil)
	defer ln.Close()

	// 1. ScanRoutesWithProfile must reject the candidate
	prof := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes := ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, port, 1, prof, nil)
	if len(nodes) != 0 {
		t.Fatalf("Case B: expected candidate REJECTED (0 nodes) for HTTP 200, got %d nodes", len(nodes))
	}

	// 2. Direct WSSHandshakeCheckDetailed check
	res := WSSHandshakeCheckDetailed("127.0.0.1", port, "sni-test.example.com", "edge-test.example.com", "/test-one", 3*time.Second)
	if res.Success {
		t.Fatalf("Case B: WSSHandshakeCheckDetailed must return Success=false for HTTP 200")
	}
	if res.HTTPStatus != 200 {
		t.Fatalf("Case B: expected HTTPStatus=200, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "http_status" {
		t.Fatalf("Case B: expected ErrorStage=http_status, got %s", res.ErrorStage)
	}
	if res.UpgradeAccepted {
		t.Fatalf("Case B: UpgradeAccepted must be false for HTTP 200")
	}
}

// TestWSS_CaseC_HTTP400Rejected verifies Case C: TCP OK + TLS OK + HTTP 400 -> candidate rejected.
func TestWSS_CaseC_HTTP400Rejected(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)

	ln, port := startDualProtocolListener(t, tlsConf,
		func(tlsConn *tls.Conn, sni, host, path, upgrade string) (int, string) {
			return 400, "Bad Request: error 101"
		}, nil)
	defer ln.Close()

	// 1. ScanRoutesWithProfile must reject the candidate
	prof := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	nodes := ScanRoutesWithProfile(ctx, []string{"127.0.0.1"}, port, 1, prof, nil)
	if len(nodes) != 0 {
		t.Fatalf("Case C: expected candidate REJECTED (0 nodes) for HTTP 400, got %d nodes", len(nodes))
	}

	// 2. Direct WSSHandshakeCheckDetailed check
	res := WSSHandshakeCheckDetailed("127.0.0.1", port, "sni-test.example.com", "edge-test.example.com", "/test-one", 3*time.Second)
	if res.Success {
		t.Fatalf("Case C: WSSHandshakeCheckDetailed must return Success=false for HTTP 400")
	}
	if res.HTTPStatus != 400 {
		t.Fatalf("Case C: expected HTTPStatus=400, got %d", res.HTTPStatus)
	}
	if res.ErrorStage != "http_status" {
		t.Fatalf("Case C: expected ErrorStage=http_status, got %s", res.ErrorStage)
	}
}

// TestWSS_CaseD_DifferentHostSNIPath verifies Host, SNI, and Path are independently sent without mutual overrides.
func TestWSS_CaseD_DifferentHostSNIPath(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)
	var capturedHost, capturedSNI, capturedPath string
	var mu sync.Mutex

	ln, port := startDualProtocolListener(t, tlsConf,
		func(tlsConn *tls.Conn, sni, host, path, upgrade string) (int, string) {
			mu.Lock()
			capturedSNI = sni
			capturedHost = host
			capturedPath = path
			mu.Unlock()
			return 101, ""
		}, nil)
	defer ln.Close()

	// Test Round 1:
	// Host: edge-test.example.com
	// SNI:  sni-test.example.com
	// Path: /test-one
	res1 := WSSHandshakeCheckDetailed("127.0.0.1", port, "sni-test.example.com", "edge-test.example.com", "/test-one", 3*time.Second)
	if !res1.Success {
		t.Fatalf("Round 1: expected handshake success, got error: %s (stage: %s)", res1.ErrorMessage, res1.ErrorStage)
	}
	mu.Lock()
	if capturedHost != "edge-test.example.com" || capturedSNI != "sni-test.example.com" || capturedPath != "/test-one" {
		t.Fatalf("Round 1 wire mismatch: Host=%s SNI=%s Path=%s", capturedHost, capturedSNI, capturedPath)
	}
	mu.Unlock()
	if res1.HostSent != "edge-test.example.com" || res1.SNISent != "sni-test.example.com" || res1.PathSent != "/test-one" {
		t.Fatalf("Round 1 result struct mismatch: %+v", res1)
	}

	// Test Round 2:
	// Host: another.example.net
	// SNI:  sni2.example.net
	// Path: /another-ws
	res2 := WSSHandshakeCheckDetailed("127.0.0.1", port, "sni2.example.net", "another.example.net", "/another-ws", 3*time.Second)
	if !res2.Success {
		t.Fatalf("Round 2: expected handshake success, got error: %s (stage: %s)", res2.ErrorMessage, res2.ErrorStage)
	}
	mu.Lock()
	if capturedHost != "another.example.net" || capturedSNI != "sni2.example.net" || capturedPath != "/another-ws" {
		t.Fatalf("Round 2 wire mismatch: Host=%s SNI=%s Path=%s", capturedHost, capturedSNI, capturedPath)
	}
	mu.Unlock()
	if res2.HostSent != "another.example.net" || res2.SNISent != "sni2.example.net" || res2.PathSent != "/another-ws" {
		t.Fatalf("Round 2 result struct mismatch: %+v", res2)
	}
}

// TestWSS_IncompleteConfigRejected verifies Step 9:
// If Host == "" or SNI == "" or Path == "", returns Success=false and ErrorStage="config" without network attempt.
func TestWSS_IncompleteConfigRejected(t *testing.T) {
	// 1. Missing Host
	resEmptyHost := WSSHandshakeCheckDetailed("127.0.0.1", 443, "sni.example.com", "", "/pyway", 3*time.Second)
	if resEmptyHost.Success {
		t.Fatalf("expected failure for empty Host")
	}
	if resEmptyHost.ErrorStage != "config" {
		t.Fatalf("expected ErrorStage=config, got %s", resEmptyHost.ErrorStage)
	}

	// 2. Missing SNI
	resEmptySNI := WSSHandshakeCheckDetailed("127.0.0.1", 443, "", "host.example.com", "/pyway", 3*time.Second)
	if resEmptySNI.Success {
		t.Fatalf("expected failure for empty SNI")
	}
	if resEmptySNI.ErrorStage != "config" {
		t.Fatalf("expected ErrorStage=config, got %s", resEmptySNI.ErrorStage)
	}

	// 3. Missing Path
	resEmptyPath := WSSHandshakeCheckDetailed("127.0.0.1", 443, "sni.example.com", "host.example.com", "", 3*time.Second)
	if resEmptyPath.Success {
		t.Fatalf("expected failure for empty Path")
	}
	if resEmptyPath.ErrorStage != "config" {
		t.Fatalf("expected ErrorStage=config, got %s", resEmptyPath.ErrorStage)
	}
}

// TestWSS_HTTPSGoodButWSSFailedRejected verifies that a route whose HTTPS connectivity is good
// but whose WSS returns non-101 is strictly rejected under ProfileGOWAYWSS.
func TestWSS_HTTPSGoodButWSSFailedRejected(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)

	// Server returns HTTP 200 for everything (good for HTTPS probe, but bad for WSS upgrade)
	ln, port := startDualProtocolListener(t, tlsConf,
		func(tlsConn *tls.Conn, sni, host, path, upgrade string) (int, string) {
			return 200, "OK HTTPS service"
		}, nil)
	defer ln.Close()

	prof := NewProfileGOWAYWSS("edge-test.example.com", "/test-one", "sni-test.example.com", port)
	cfg := ProbeConfig{Profile: prof}
	NormalizeProbeConfig(&cfg)

	target := ResolveProbeTarget(cfg, "127.0.0.1", port)
	if target.ProfileType != ProfileGOWAYWSS {
		t.Fatalf("expected target ProfileType GOWAY-WSS, got %s", target.ProfileType)
	}

	// Layered probe must fail L2 WSS handshake
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sched := NewProbeScheduler(NewRouteStore(), cfg)
	res := sched.ExecuteLayeredProbeWithSnapshot(ctx, target, cfg, false, false)
	if res.HandshakeSuccess {
		t.Fatalf("expected HandshakeSuccess=false when WSS returns HTTP 200")
	}
	if !strings.Contains(res.Error, "http_status") {
		t.Fatalf("expected error mentioning http_status, got: %s", res.Error)
	}
}

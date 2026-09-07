package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func setupAPITestData() {
	GlobalRouteStore.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID("162.159.192.1", 443),
		IP:                 "162.159.192.1",
		Port:               443,
		Colo:               "HKG",
		Tier:               TierActive,
		Health:             HealthHealthy,
		RTT:                42.5,
		PacketLoss:         0.0,
		Jitter:             2.1,
		DownloadSpeed:      68.4,
		SingleSpeed:        65.2,
		MinSpeed:           52.8,
		Stability:          95.4,
		HandshakeSuccess:   true,
		FinalScore:         91.3,
		ConsecutiveSuccess: 10,
		LastTested:         time.Now(),
		Timestamp:          time.Now(),
	})

	GlobalRouteStore.UpsertRoute(RouteMetrics{
		ID:                 GenerateRouteID("162.159.193.1", 443),
		IP:                 "162.159.193.1",
		Port:               443,
		Colo:               "NRT",
		Tier:               TierStandby,
		Health:             HealthHealthy,
		RTT:                55.0,
		PacketLoss:         0.0,
		Jitter:             3.0,
		DownloadSpeed:      50.0,
		SingleSpeed:        48.0,
		MinSpeed:           40.0,
		Stability:          90.0,
		HandshakeSuccess:   true,
		FinalScore:         85.0,
		ConsecutiveSuccess: 5,
		LastTested:         time.Now(),
		Timestamp:          time.Now(),
	})
}

func TestAPIHealth(t *testing.T) {
	setupAPITestData()

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()

	handleAPIHealth(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if data["status"] != "ok" || data["service"] != "CFST Route Quality Probe" {
		t.Fatalf("unexpected health data: %+v", data)
	}
}

func TestAPIRoutesAndBest(t *testing.T) {
	setupAPITestData()

	// 1. GET /api/routes
	req := httptest.NewRequest(http.MethodGet, "/api/routes", nil)
	w := httptest.NewRecorder()
	handleAPIRoutes(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/routes, got %d", w.Result().StatusCode)
	}

	var routes []RouteMetrics
	if err := json.NewDecoder(w.Result().Body).Decode(&routes); err != nil {
		t.Fatalf("failed to decode routes: %v", err)
	}
	if len(routes) < 2 {
		t.Fatalf("expected at least 2 routes, got %d", len(routes))
	}

	// 2. GET /api/routes/best?limit=1
	reqBest := httptest.NewRequest(http.MethodGet, "/api/routes/best?limit=1", nil)
	wBest := httptest.NewRecorder()
	handleAPIBestRoutes(wBest, reqBest)
	if wBest.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/routes/best, got %d", wBest.Result().StatusCode)
	}

	var best []RouteMetrics
	if err := json.NewDecoder(wBest.Result().Body).Decode(&best); err != nil {
		t.Fatalf("failed to decode best routes: %v", err)
	}
	if len(best) != 1 || best[0].IP != "162.159.192.1" {
		t.Fatalf("expected 162.159.192.1 as top route, got %+v", best)
	}

	// 3. GET /api/routes/metrics
	reqMetrics := httptest.NewRequest(http.MethodGet, "/api/routes/metrics", nil)
	wMetrics := httptest.NewRecorder()
	handleAPIMetricsSummary(wMetrics, reqMetrics)
	if wMetrics.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /api/routes/metrics, got %d", wMetrics.Result().StatusCode)
	}
}

func TestAPIRouteDetailAndTierUpdate(t *testing.T) {
	setupAPITestData()

	// 1. GET /api/routes/162.159.192.1
	req := httptest.NewRequest(http.MethodGet, "/api/routes/162.159.192.1", nil)
	w := httptest.NewRecorder()
	handleAPIRoutes(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for route detail, got %d", w.Result().StatusCode)
	}

	// 2. POST /api/routes/tier
	body := bytes.NewBufferString(`{"ip": "162.159.193.1", "tier": "ACTIVE"}`)
	reqTier := httptest.NewRequest(http.MethodPost, "/api/routes/tier", body)
	wTier := httptest.NewRecorder()
	handleAPIRouteTier(wTier, reqTier)
	if wTier.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for tier update, got %d", wTier.Result().StatusCode)
	}

	rec, _ := GlobalRouteStore.Get("162.159.193.1")
	if rec.Metrics.Tier != TierActive {
		t.Fatalf("expected updated tier ACTIVE, got %s", rec.Metrics.Tier)
	}
}

func TestAPIConfig(t *testing.T) {
	// 1. GET /api/config
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	handleAPIConfig(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for config get, got %d", w.Result().StatusCode)
	}

	// 2. POST /api/config to update mode to peak
	body := bytes.NewBufferString(`{"mode": "peak", "active_interval_sec": 15}`)
	reqUpdate := httptest.NewRequest(http.MethodPost, "/api/config", body)
	wUpdate := httptest.NewRecorder()
	handleAPIConfig(wUpdate, reqUpdate)
	if wUpdate.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for config update, got %d", wUpdate.Result().StatusCode)
	}

	if GlobalScoreEngine.GetMode() != ModePeak {
		t.Fatalf("expected updated mode peak, got %s", GlobalScoreEngine.GetMode())
	}
	if GlobalProbeScheduler.GetConfig().ActiveInterval != 15*time.Second {
		t.Fatalf("expected updated active interval 15s, got %v", GlobalProbeScheduler.GetConfig().ActiveInterval)
	}
}

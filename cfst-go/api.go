package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var appStartTime = time.Now()

// RegisterAPIRoutes mounts GoPass-facing REST API endpoints onto the HTTP mux.
func RegisterAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/health", handleAPIHealth)
	mux.HandleFunc("/api/routes", handleAPIRoutes)
	mux.HandleFunc("/api/routes/best", handleAPIBestRoutes)
	mux.HandleFunc("/api/routes/metrics", handleAPIMetricsSummary)
	mux.HandleFunc("/api/routes/tier", handleAPIRouteTier)
	mux.HandleFunc("/api/probe", handleAPIProbe)
	mux.HandleFunc("/api/config", handleAPIConfig)
}

func init() {
	RegisterAPIRoutes(http.DefaultServeMux)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := GlobalRouteStore.GetAll()
	var healthyCount, activeCount, standbyCount int
	for _, m := range all {
		if m.Health == HealthHealthy {
			healthyCount++
		}
		if m.Tier == TierActive {
			activeCount++
		} else if m.Tier == TierStandby {
			standbyCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"service":         "CFST Route Quality Probe",
		"version":         "v2.0.0",
		"uptime_seconds":  int64(time.Since(appStartTime).Seconds()),
		"score_mode":      GlobalScoreEngine.GetMode(),
		"total_routes":    len(all),
		"healthy_routes":  healthyCount,
		"active_routes":   activeCount,
		"standby_routes":  standbyCount,
		"timestamp":       time.Now().Format(time.RFC3339),
	})
}

func handleAPIRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/routes")
	path = strings.TrimPrefix(path, "/")

	// Sub-route handling: /api/routes/{id_or_ip}
	if path != "" && path != "best" && path != "metrics" && path != "tier" {
		handleAPIRouteDetail(w, r, path)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	colo := strings.ToUpper(q.Get("colo"))
	tier := RouteTier(strings.ToUpper(q.Get("tier")))
	health := RouteHealth(strings.ToUpper(q.Get("health")))
	limitStr := q.Get("limit")

	all := GlobalRouteStore.GetAll()
	var filtered []RouteMetrics
	for _, m := range all {
		if colo != "" && m.Colo != colo {
			continue
		}
		if tier != "" && m.Tier != tier {
			continue
		}
		if health != "" && m.Health != health {
			continue
		}
		filtered = append(filtered, m)
	}

	if limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}
	}

	if filtered == nil {
		filtered = []RouteMetrics{}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func handleAPIBestRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	limit := 5
	if l := q.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	colo := strings.ToUpper(q.Get("colo"))

	best := GlobalRouteStore.GetBest(limit, colo)
	if best == nil {
		best = []RouteMetrics{}
	}
	writeJSON(w, http.StatusOK, best)
}

// Lightweight metrics summary for fast GoPass controller polling
type RouteMetricSummary struct {
	ID          string      `json:"id"`
	IP          string      `json:"ip"`
	Port        int         `json:"port"`
	Colo        string      `json:"colo"`
	Tier        RouteTier   `json:"tier"`
	Health      RouteHealth `json:"health"`
	RTT         float64     `json:"rtt"`
	PacketLoss  float64     `json:"packet_loss"`
	Jitter      float64     `json:"jitter"`
	SingleSpeed float64     `json:"single_speed"`
	MinSpeed    float64     `json:"min_speed"`
	FinalScore  float64     `json:"final_score"`
	Timestamp   time.Time   `json:"timestamp"`
}

func handleAPIMetricsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := GlobalRouteStore.GetAll()
	summaries := make([]RouteMetricSummary, len(all))
	for i, m := range all {
		summaries[i] = RouteMetricSummary{
			ID:          m.ID,
			IP:          m.IP,
			Port:        m.Port,
			Colo:        m.Colo,
			Tier:        m.Tier,
			Health:      m.Health,
			RTT:         m.RTT,
			PacketLoss:  m.PacketLoss,
			Jitter:      m.Jitter,
			SingleSpeed: m.SingleSpeed,
			MinSpeed:    m.MinSpeed,
			FinalScore:  m.FinalScore,
			Timestamp:   m.Timestamp,
		}
	}
	writeJSON(w, http.StatusOK, summaries)
}

func handleAPIRouteDetail(w http.ResponseWriter, r *http.Request, idOrIP string) {
	rec, exists := GlobalRouteStore.Get(idOrIP)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Route not found"})
		return
	}

	windows := GlobalRouteStore.GetHistoryWindows(idOrIP)
	peakHours := GlobalRouteStore.GetPeakHours(idOrIP)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"metrics":    rec.Metrics,
		"ewma_short": rec.EWMA.ShortSnapshot(),
		"ewma_long":  rec.EWMA.LongSnapshot(),
		"history":    windows,
		"peak_hours": peakHours,
	})
}

type SetTierRequest struct {
	IP   string    `json:"ip"`
	Tier RouteTier `json:"tier"`
}

func handleAPIRouteTier(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SetTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	req.Tier = RouteTier(strings.ToUpper(string(req.Tier)))
	switch req.Tier {
	case TierActive, TierStandby, TierCandidate, TierFailed:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid tier; must be ACTIVE, STANDBY, CANDIDATE, or FAILED"})
		return
	}

	if !GlobalRouteStore.SetTier(req.IP, req.Tier) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Route IP not found in managed pool"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "updated",
		"ip":     req.IP,
		"tier":   req.Tier,
	})
}

type ProbeRequest struct {
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	SpeedTest bool   `json:"speed_test"`
}

func handleAPIProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ProbeRequest
	if r.Header.Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(r.Body).Decode(&req)
	} else {
		req.IP = r.URL.Query().Get("ip")
		req.SpeedTest = r.URL.Query().Get("speed_test") == "true"
	}

	if req.IP == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "IP is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	metrics, err := GlobalProbeScheduler.ProbeOnce(ctx, req.IP, req.SpeedTest)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, metrics)
}

type ConfigUpdateRequest struct {
	Mode              *ScoreMode `json:"mode,omitempty"`
	ActiveIntervalSec *int       `json:"active_interval_sec,omitempty"`
	StandbyIntervalSec *int      `json:"standby_interval_sec,omitempty"`
}

func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := GlobalProbeScheduler.GetConfig()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"score_mode":             GlobalScoreEngine.GetMode(),
			"active_interval_sec":    int(cfg.ActiveInterval.Seconds()),
			"standby_interval_sec":   int(cfg.StandbyInterval.Seconds()),
			"candidate_interval_sec": int(cfg.CandidateInterval.Seconds()),
			"failed_interval_sec":    int(cfg.FailedInterval.Seconds()),
			"max_concurrent":         cfg.MaxConcurrent,
			"max_speed_tests":        cfg.MaxSpeedTests,
		})
		return
	}

	if r.Method == http.MethodPost {
		var req ConfigUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
			return
		}

		if req.Mode != nil {
			m := ScoreMode(strings.ToLower(string(*req.Mode)))
			if m == ModeNormal || m == ModePeak {
				GlobalScoreEngine.SetMode(m)
			}
		}

		cfg := GlobalProbeScheduler.GetConfig()
		if req.ActiveIntervalSec != nil && *req.ActiveIntervalSec > 0 {
			cfg.ActiveInterval = time.Duration(*req.ActiveIntervalSec) * time.Second
		}
		if req.StandbyIntervalSec != nil && *req.StandbyIntervalSec > 0 {
			cfg.StandbyInterval = time.Duration(*req.StandbyIntervalSec) * time.Second
		}
		GlobalProbeScheduler.UpdateConfig(cfg)

		writeJSON(w, http.StatusOK, map[string]string{"status": "config updated"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

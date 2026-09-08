package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
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
	mux.HandleFunc("/api/discovery/status", handleAPIDiscoveryStatus)
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

	cfg := GlobalProbeScheduler.GetConfig()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "ok",
		"service":        "CFST Route Quality Probe",
		"version":        "v2.1.4",
		"uptime_seconds": int64(time.Since(appStartTime).Seconds()),
		"score_mode":     GlobalScoreEngine.GetMode(),
		"profile":        cfg.Profile.Type,
		"profile_type":   cfg.Profile.Type,
		"test_url":       cfg.Profile.TestURL,
		"sni":            cfg.Profile.SNI,
		"host":           cfg.Profile.Host,
		"path":           cfg.Profile.Path,
		"protocol":       cfg.Profile.Protocol,
		"total_routes":   len(all),
		"healthy_routes": healthyCount,
		"active_routes":  activeCount,
		"standby_routes": standbyCount,
		"timestamp":      time.Now().Format(time.RFC3339),
	})
}

func handleAPIRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/routes")
	path = strings.TrimPrefix(path, "/")

	// Sub-route handling: /api/routes/{ip}/[history|peak|samples]
	if path != "" && path != "best" && path != "metrics" && path != "tier" {
		parts := strings.Split(path, "/")
		ip := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}

		rec, exists := GlobalRouteStore.Get(ip)
		if !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Route not found"})
			return
		}

		switch sub {
		case "history":
			windows := GlobalRouteStore.GetHistoryWindows(ip)
			writeJSON(w, http.StatusOK, windows)
			return
		case "peak":
			peakHours := GlobalRouteStore.GetPeakHours(ip)
			writeJSON(w, http.StatusOK, peakHours)
			return
		case "samples":
			writeJSON(w, http.StatusOK, rec.Samples)
			return
		default:
			handleAPIRouteDetail(w, r, ip)
			return
		}
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

type SpeedSummary struct {
	Avg    float64 `json:"avg"`
	Median float64 `json:"median"`
	P10    float64 `json:"p10"`
	Min    float64 `json:"min"`
}

type StallSummary struct {
	Count         int     `json:"count"`
	Rate          float64 `json:"rate"`
	TotalDuration float64 `json:"total_duration"`
}

// Lightweight metrics summary for fast GoPass controller polling
type RouteMetricSummary struct {
	ID             string              `json:"id"`
	IP             string              `json:"ip"`
	Port           int                 `json:"port"`
	Colo           string              `json:"colo"`
	Tier           RouteTier           `json:"tier"`
	Health         RouteHealth         `json:"health"`
	StabilityGrade StabilityGrade      `json:"stability_grade"`
	Recommendation RouteRecommendation `json:"recommendation"`
	Reasons        []string            `json:"reasons,omitempty"`
	RTT            float64             `json:"rtt"`
	PacketLoss     float64             `json:"packet_loss"`
	Jitter         float64             `json:"jitter"`
	SingleSpeed    float64             `json:"single_speed"`
	MinSpeed       float64             `json:"min_speed"`
	P10Speed       float64             `json:"p10_speed"`
	Confidence     float64             `json:"confidence"`
	PeakHourScore  float64             `json:"peak_hour_score"`
	FinalScore     float64             `json:"final_score"`
	Speed          SpeedSummary        `json:"speed"`
	Stalls         StallSummary        `json:"stalls"`
	Timestamp      time.Time           `json:"timestamp"`
}

func handleAPIMetricsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := GlobalRouteStore.GetAll()
	summaries := make([]RouteMetricSummary, len(all))
	for i, m := range all {
		effectiveSpeed := m.SingleSpeed
		if effectiveSpeed <= 0 {
			effectiveSpeed = m.DownloadSpeed
		}
		median := m.MedianSpeed
		if median <= 0 {
			median = effectiveSpeed
		}
		p10 := m.P10Speed
		if p10 <= 0 {
			p10 = m.MinSpeed
		}

		summaries[i] = RouteMetricSummary{
			ID:             m.ID,
			IP:             m.IP,
			Port:           m.Port,
			Colo:           m.Colo,
			Tier:           m.Tier,
			Health:         m.Health,
			StabilityGrade: m.StabilityGrade,
			Recommendation: m.Recommendation,
			Reasons:        m.RecommendationReasons,
			RTT:            m.RTT,
			PacketLoss:     m.PacketLoss,
			Jitter:         m.Jitter,
			SingleSpeed:    m.SingleSpeed,
			MinSpeed:       m.MinSpeed,
			P10Speed:       p10,
			Confidence:     m.Confidence,
			PeakHourScore:  m.PeakHourScore,
			FinalScore:     m.FinalScore,
			Speed: SpeedSummary{
				Avg:    math.Round(effectiveSpeed*10) / 10,
				Median: math.Round(median*10) / 10,
				P10:    math.Round(p10*10) / 10,
				Min:    math.Round(m.MinSpeed*10) / 10,
			},
			Stalls: StallSummary{
				Count:         m.StallCount,
				Rate:          math.Round(m.StallRate*1000) / 1000,
				TotalDuration: math.Round(m.TotalStallDuration*10) / 10,
			},
			Timestamp: m.Timestamp,
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

	effectiveSpeed := rec.Metrics.SingleSpeed
	if effectiveSpeed <= 0 {
		effectiveSpeed = rec.Metrics.DownloadSpeed
	}
	median := rec.Metrics.MedianSpeed
	if median <= 0 {
		median = effectiveSpeed
	}
	p10 := rec.Metrics.P10Speed
	if p10 <= 0 {
		p10 = rec.Metrics.MinSpeed
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"metrics":         rec.Metrics,
		"recommendation":  rec.Metrics.Recommendation,
		"stability_grade": rec.Metrics.StabilityGrade,
		"confidence":      rec.Metrics.Confidence,
		"final_score":     rec.Metrics.FinalScore,
		"speed": SpeedSummary{
			Avg:    math.Round(effectiveSpeed*10) / 10,
			Median: math.Round(median*10) / 10,
			P10:    math.Round(p10*10) / 10,
			Min:    math.Round(rec.Metrics.MinSpeed*10) / 10,
		},
		"stalls": StallSummary{
			Count:         rec.Metrics.StallCount,
			Rate:          math.Round(rec.Metrics.StallRate*1000) / 1000,
			TotalDuration: math.Round(rec.Metrics.TotalStallDuration*10) / 10,
		},
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
	Mode                 *ScoreMode        `json:"mode,omitempty"`
	ProfileType          *ProbeProfileType `json:"profile_type,omitempty"`
	TestURL              *string           `json:"test_url,omitempty"`
	SNI                  *string           `json:"sni,omitempty"`
	Host                 *string           `json:"host,omitempty"`
	Path                 *string           `json:"path,omitempty"`
	Port                 *int              `json:"port,omitempty"`
	ActiveIntervalSec    *int              `json:"active_interval_sec,omitempty"`
	StandbyIntervalSec   *int              `json:"standby_interval_sec,omitempty"`
	L3ProbeCycle         *int              `json:"l3_probe_cycle,omitempty"`
	FullSpeedCycle       *int              `json:"full_speed_cycle,omitempty"`
	DiscoveryEnabled     *bool             `json:"discovery_enabled,omitempty"`
	DiscoveryIntervalSec *int              `json:"discovery_interval_sec,omitempty"`
	DiscoveryScanCount   *int              `json:"discovery_scan_count,omitempty"`
}

func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := GlobalProbeScheduler.GetConfig()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"version":                "v2.1.4",
			"score_mode":             GlobalScoreEngine.GetMode(),
			"profile":                cfg.Profile,
			"profile_type":           cfg.Profile.Type,
			"test_url":               cfg.Profile.TestURL,
			"sni":                    cfg.Profile.SNI,
			"host":                   cfg.Profile.Host,
			"path":                   cfg.Profile.Path,
			"protocol":               cfg.Profile.Protocol,
			"active_interval_sec":    int(cfg.ActiveInterval.Seconds()),
			"standby_interval_sec":   int(cfg.StandbyInterval.Seconds()),
			"candidate_interval_sec": int(cfg.CandidateInterval.Seconds()),
			"failed_interval_sec":    int(cfg.FailedInterval.Seconds()),
			"max_concurrent":         cfg.MaxConcurrent,
			"max_speed_tests":        cfg.MaxSpeedTests,
			"l3_probe_cycle":         cfg.L3ProbeCycle,
			"full_speed_cycle":       cfg.FullSpeedCycle,
			"discovery_enabled":      cfg.DiscoveryEnabled,
			"discovery_interval_sec": int(cfg.DiscoveryInterval.Seconds()),
			"discovery_scan_count":   cfg.DiscoveryScanCount,
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
		if req.ProfileType != nil {
			switch *req.ProfileType {
			case ProfileCFST:
				cfg.Profile = NewProfileCFST()
			case ProfileGOWAYWSS:
				cfg.Profile = NewProfileGOWAYWSS("", "", "", 443)
			case ProfileCustom:
				cfg.Profile = NewProfileCustom("", "", 443)
			}
		}
		if req.Port != nil && *req.Port > 0 {
			cfg.Profile.Port = *req.Port
		}
		if req.TestURL != nil {
			cfg.Profile.TestURL = *req.TestURL
			if u, err := url.Parse(*req.TestURL); err == nil && u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 {
					cfg.Profile.Port = p
				}
			}
		}
		if req.SNI != nil {
			cfg.Profile.SNI = *req.SNI
		}
		if req.Host != nil {
			cfg.Profile.Host = *req.Host
		}
		if req.Path != nil {
			cfg.Profile.Path = *req.Path
		}
		if req.ActiveIntervalSec != nil && *req.ActiveIntervalSec > 0 {
			cfg.ActiveInterval = time.Duration(*req.ActiveIntervalSec) * time.Second
		}
		if req.StandbyIntervalSec != nil && *req.StandbyIntervalSec > 0 {
			cfg.StandbyInterval = time.Duration(*req.StandbyIntervalSec) * time.Second
		}
		if req.L3ProbeCycle != nil && *req.L3ProbeCycle > 0 {
			cfg.L3ProbeCycle = *req.L3ProbeCycle
		}
		if req.FullSpeedCycle != nil && *req.FullSpeedCycle > 0 {
			cfg.FullSpeedCycle = *req.FullSpeedCycle
		}
		if req.DiscoveryEnabled != nil {
			cfg.DiscoveryEnabled = *req.DiscoveryEnabled
		}
		if req.DiscoveryIntervalSec != nil && *req.DiscoveryIntervalSec > 0 {
			cfg.DiscoveryInterval = time.Duration(*req.DiscoveryIntervalSec) * time.Second
		}
		if req.DiscoveryScanCount != nil && *req.DiscoveryScanCount > 0 {
			cfg.DiscoveryScanCount = *req.DiscoveryScanCount
		}
		GlobalProbeScheduler.UpdateConfig(cfg)

		writeJSON(w, http.StatusOK, map[string]string{"status": "config updated"})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func handleAPIDiscoveryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := GlobalDiscoveryManager.GetStatus()
	writeJSON(w, http.StatusOK, status)
}

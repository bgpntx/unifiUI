package main

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed public
var staticFS embed.FS

// ─── Configuration ───────────────────────────────────────────

type config struct {
	UDRBase   string
	SiteID    string
	APIKey    string
	Port      string
	UnsafeTLS bool
}

func loadConfig() config {
	c := config{
		UDRBase:   envOr("UDR_BASE", "https://192.168.69.1"),
		SiteID:    envOr("UNIFI_SITE_ID", "88f7af54-98f8-306a-a1c7-c9349722b1f6"),
		APIKey:    os.Getenv("UNIFI_API_KEY"),
		Port:      envOr("PORT", "5173"),
		UnsafeTLS: os.Getenv("UNSAFE_TLS") == "1",
	}
	if c.APIKey == "" {
		log.Fatal("[FATAL] UNIFI_API_KEY is not set. Export it in your environment.")
	}
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── UDR API Client ──────────────────────────────────────────

type udrClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newUDRClient(cfg config) *udrClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.UnsafeTLS, //nolint:gosec // user-configurable for self-signed UDR certs
		},
		MaxIdleConns:      10,
		IdleConnTimeout:   30 * time.Second,
		DisableKeepAlives: false,
	}
	return &udrClient{
		baseURL: cfg.UDRBase,
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}
}

// doRequest performs a generic request to the UDR Integration v1 API.
func (u *udrClient) doRequest(method, pathname string, body io.Reader) (json.RawMessage, error) {
	url := u.baseURL + "/proxy/network/integration/v1" + pathname
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", u.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%d %s: %s", resp.StatusCode, resp.Status, string(data))
	}
	return json.RawMessage(data), nil
}

func (u *udrClient) get(path string) (json.RawMessage, error) {
	return u.doRequest("GET", path, nil)
}

func (u *udrClient) post(path string, body io.Reader) (json.RawMessage, error) {
	return u.doRequest("POST", path, body)
}

// getLegacySiteName maps Integration v1 site UUID -> legacy short name (e.g., "default").
func (u *udrClient) getLegacySiteName(siteID string) string {
	raw, err := u.get("/sites")
	if err != nil {
		return "default"
	}
	var resp struct {
		Data []struct {
			ID                string `json:"id"`
			InternalReference string `json:"internalReference"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return "default"
	}
	for _, s := range resp.Data {
		if s.ID == siteID && s.InternalReference != "" {
			return s.InternalReference
		}
	}
	// fallback: first site or 'default'
	if len(resp.Data) > 0 && resp.Data[0].InternalReference != "" {
		return resp.Data[0].InternalReference
	}
	return "default"
}

// getWanHealth fetches WAN health via legacy endpoint.
// Many UniFi builds expose rx_bytes-r / tx_bytes-r on /api/s/<site>/stat/health under the 'wan' subsystem.
// We go through the same console proxy but use the legacy path (not Integration v1).
func (u *udrClient) getWanHealth(siteID string) (json.RawMessage, error) {
	legacySite := u.getLegacySiteName(siteID)
	url := u.baseURL + "/proxy/network/api/s/" + legacySite + "/stat/health"

	// 1) Try with API key header
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-KEY", u.apiKey)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 2) If forbidden/unauthorized, retry WITHOUT the key header (some UniFi builds require legacy auth here)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		resp.Body.Close()
		req2, _ := http.NewRequest("GET", url, nil)
		req2.Header.Set("Accept", "application/json")
		resp, err = u.httpClient.Do(req2)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%d %s: %s", resp.StatusCode, resp.Status, string(data))
	}
	return json.RawMessage(data), nil
}

// ─── Rate Limiter (120 req/min per IP) ───────────────────────

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*rateEntry
	max     int
	window  time.Duration
}

type rateEntry struct {
	start time.Time
	count int
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		clients: make(map[string]*rateEntry),
		max:     max,
		window:  window,
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, ok := rl.clients[ip]
	if !ok || now.Sub(entry.start) > rl.window {
		rl.clients[ip] = &rateEntry{start: now, count: 1}
		return true
	}
	entry.count++
	return entry.count <= rl.max
}

// ─── HTTP Handlers ───────────────────────────────────────────

type server struct {
	cfg    config
	udr    *udrClient
	siteID string // mutable default site (in-process only)
	siteMu sync.RWMutex
}

func (s *server) getSiteID(r *http.Request) string {
	if id := r.URL.Query().Get("siteId"); id != "" {
		return id
	}
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.siteID
}

func (s *server) setSiteID(id string) {
	s.siteMu.Lock()
	defer s.siteMu.Unlock()
	s.siteID = id
}

// Health endpoint — перевіряє з'єднання з UDR
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	_, err := s.udr.get("/sites")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "udr": "unreachable", "error": err.Error(), "ts": time.Now().UnixMilli(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "udr": "reachable", "ts": time.Now().UnixMilli(),
	})
}

// List sites
func (s *server) handleGetSites(w http.ResponseWriter, _ *http.Request) {
	data, err := s.udr.get("/sites")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// Allow the UI to change default site (kept in-process)
func (s *server) handleSetSite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SiteID string `json:"siteId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.SiteID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "siteId required"})
		return
	}
	s.setSiteID(strings.TrimSpace(body.SiteID))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "siteId": body.SiteID})
}

// List clients for a site (optional ?siteId= overrides default)
func (s *server) handleGetClients(w http.ResponseWriter, r *http.Request) {
	siteID := s.getSiteID(r)
	data, err := s.udr.get("/sites/" + siteID + "/clients")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// List devices for a site
func (s *server) handleGetDevices(w http.ResponseWriter, r *http.Request) {
	siteID := s.getSiteID(r)
	data, err := s.udr.get("/sites/" + siteID + "/devices")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// Current WAN rates (bytes/sec) + basic status
func (s *server) handleWanHealth(w http.ResponseWriter, r *http.Request) {
	siteID := s.getSiteID(r)
	raw, err := s.udr.getWanHealth(siteID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
		return
	}

	// Find 'wan' subsystem
	var wan map[string]any
	for _, item := range resp.Data {
		if item["subsystem"] == "wan" {
			wan = item
			break
		}
	}
	if wan == nil {
		wan = map[string]any{}
	}

	result := map[string]any{
		"ts":          time.Now().UnixMilli(),
		"rx_bps":      wan["rx_bytes-r"],
		"tx_bps":      wan["tx_bytes-r"],
		"wan_ip":      wan["wan_ip"],
		"status":      wan["status"],
		"legacy_site": s.udr.getLegacySiteName(siteID),
	}
	if result["rx_bps"] == nil && result["tx_bps"] == nil {
		result["note"] = "no_wan_rate_data"
	}
	writeJSON(w, http.StatusOK, result)
}

// Authorize guest client (External Hotspot action)
func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	// Extract clientId from path: /api/clients/{clientId}/authorize
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/clients/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "clientId required"})
		return
	}
	clientID := parts[0]
	siteID := s.getSiteID(r)

	var reqBody struct {
		TimeLimitMinutes     int `json:"timeLimitMinutes"`
		DataUsageLimitMBytes int `json:"dataUsageLimitMBytes"`
		TxRateLimitKbps      int `json:"txRateLimitKbps"`
		RxRateLimitKbps      int `json:"rxRateLimitKbps"`
	}
	// defaults
	reqBody.TimeLimitMinutes = 120
	json.NewDecoder(r.Body).Decode(&reqBody)

	payload := map[string]any{
		"action":               "AUTHORIZE_GUEST_ACCESS",
		"timeLimitMinutes":     reqBody.TimeLimitMinutes,
		"dataUsageLimitMBytes": reqBody.DataUsageLimitMBytes,
		"txRateLimitKbps":      reqBody.TxRateLimitKbps,
		"rxRateLimitKbps":      reqBody.RxRateLimitKbps,
	}
	bodyBytes, _ := json.Marshal(payload)

	data, err := s.udr.post(
		"/sites/"+siteID+"/clients/"+clientID+"/actions",
		strings.NewReader(string(bodyBytes)),
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// ─── Helpers ─────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ─── Middleware ──────────────────────────────────────────────

// loggingMiddleware logs all requests except noisy WAN polling.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy WAN polling from cluttering logs
		if r.URL.Path != "/api/wan/health" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware restricts CORS to localhost.
func corsMiddleware(port string) func(http.Handler) http.Handler {
	allowedOrigins := map[string]bool{
		"http://localhost:" + port: true,
		"http://127.0.0.1:" + port: true,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if allowedOrigins[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimitMiddleware applies per-IP rate limiting.
func rateLimitMiddleware(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr
			if idx := strings.LastIndex(ip, ":"); idx != -1 {
				ip = ip[:idx]
			}
			if !rl.allow(ip) {
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// chain applies middleware in order.
func chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// ─── Healthcheck (self-contained for scratch Docker) ─────────

func runHealthcheck(port string) {
	resp, err := http.Get("http://localhost:" + port + "/health")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		os.Exit(0)
	}
	os.Exit(1)
}

// ─── Main ────────────────────────────────────────────────────

func main() {
	// Docker healthcheck mode
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck(envOr("PORT", "5173"))
		return
	}

	cfg := loadConfig()
	udr := newUDRClient(cfg)

	srv := &server{
		cfg:    cfg,
		udr:    udr,
		siteID: cfg.SiteID,
	}

	rl := newRateLimiter(120, time.Minute)

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.HandleFunc("GET /api/sites", srv.handleGetSites)
	mux.HandleFunc("POST /api/site", srv.handleSetSite)
	mux.HandleFunc("GET /api/clients", srv.handleGetClients)
	mux.HandleFunc("GET /api/devices", srv.handleGetDevices)
	mux.HandleFunc("GET /api/wan/health", srv.handleWanHealth)
	mux.HandleFunc("POST /api/clients/{clientId}/authorize", srv.handleAuthorize)

	// Static files (embedded)
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	handler := chain(mux,
		rateLimitMiddleware(rl),
		corsMiddleware(cfg.Port),
		loggingMiddleware,
	)

	httpSrv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
	}()

	log.Printf("UI running: http://localhost:%s", cfg.Port)
	log.Printf("Using UDR base %s · default site %s · UNSAFE_TLS=%v", cfg.UDRBase, cfg.SiteID, cfg.UnsafeTLS)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[FATAL] %v", err)
	}
}

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// ─── .env loader ─────────────────────────────────────────────

// loadDotEnv reads a .env file and sets environment variables.
// Existing env vars take precedence (won't be overwritten).
// Supports comments (#) and KEY=VALUE format.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// existing env vars take precedence
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// ─── Configuration ───────────────────────────────────────────

type config struct {
	UDRBase   string
	SiteID    string
	APIKey    string
	Port      string
	UnsafeTLS bool
}

func loadConfig() config {
	// Load .env file if present (env vars take precedence)
	loadDotEnv(".env")

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

// getDeviceStat fetches per-device stats via legacy stat/device endpoint.
// Gateway device contains per-uplink (WAN) traffic data.
func (u *udrClient) getDeviceStat(siteID string) (json.RawMessage, error) {
	legacySite := u.getLegacySiteName(siteID)
	url := u.baseURL + "/proxy/network/api/s/" + legacySite + "/stat/device"

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

// Current WAN rates (bytes/sec) per-WAN via device stats
func (s *server) handleWanHealth(w http.ResponseWriter, r *http.Request) {
	siteID := s.getSiteID(r)

	// 1) Fetch Integration v1 WAN names (Astra, LinkCOM, etc.)
	wanNames := map[int]string{} // index -> human name
	wansRaw, err := s.udr.get("/sites/" + siteID + "/wans")
	if err == nil {
		var wansResp struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		if json.Unmarshal(wansRaw, &wansResp) == nil {
			for i, w := range wansResp.Data {
				wanNames[i] = w.Name
			}
		}
	}

	// 2) Fetch per-WAN traffic from legacy stat/device (gateway wan1/wan2 objects)
	var wans []map[string]any
	devRaw, err := s.udr.getDeviceStat(siteID)
	if err == nil {
		var devResp struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(devRaw, &devResp) == nil {
			for _, dev := range devResp.Data {
				devType, _ := dev["type"].(string)
				if devType != "ugw" && devType != "udm" && devType != "uxg" {
					continue
				}
				// Extract wan1, wan2, wan3... from gateway device
				for i := 1; i <= 4; i++ {
					key := fmt.Sprintf("wan%d", i)
					wanData, ok := dev[key].(map[string]any)
					if !ok {
						continue
					}
					// Skip disabled WANs
					if enabled, ok := wanData["enable"].(bool); ok && !enabled {
						continue
					}

					name := wanNames[i-1]
					if name == "" {
						name = strings.ToUpper(key)
					}

					entry := map[string]any{
						"name":         name,
						"key":          key,
						"ip":           wanData["ip"],
						"latency":      wanData["latency"],
						"availability": wanData["availability"],
					}

					// Per-WAN traffic rates
					if rx, ok := wanData["rx_bytes-r"].(float64); ok {
						entry["rx_bps"] = rx
					}
					if tx, ok := wanData["tx_bytes-r"].(float64); ok {
						entry["tx_bps"] = tx
					}
					// Fallback: some builds only have "bytes-r" (combined)
					if entry["rx_bps"] == nil && entry["tx_bps"] == nil {
						if br, ok := wanData["bytes-r"].(float64); ok {
							entry["bytes_r"] = br
						}
					}

					wans = append(wans, entry)
				}
				break // only need first gateway
			}
		}
	}

	// 3) Fallback: legacy stat/health aggregate (in case device stats failed)
	if len(wans) == 0 {
		raw, err := s.udr.getWanHealth(siteID)
		if err == nil {
			var resp struct {
				Data []map[string]any `json:"data"`
			}
			if json.Unmarshal(raw, &resp) == nil {
				for _, item := range resp.Data {
					if item["subsystem"] == "wan" {
						wans = append(wans, map[string]any{
							"name":   "WAN",
							"key":    "wan",
							"rx_bps": item["rx_bytes-r"],
							"tx_bps": item["tx_bytes-r"],
							"ip":     item["wan_ip"],
						})
						break
					}
				}
			}
		}
	}

	result := map[string]any{
		"ts":   time.Now().UnixMilli(),
		"wans": wans,
	}
	writeJSON(w, http.StatusOK, result)
}

// Debug: raw WAN data from both legacy and Integration v1
func (s *server) handleWanRaw(w http.ResponseWriter, r *http.Request) {
	siteID := s.getSiteID(r)
	result := map[string]any{"siteId": siteID}

	// Legacy stat/health — all subsystems
	raw, err := s.udr.getWanHealth(siteID)
	if err != nil {
		result["legacy_error"] = err.Error()
	} else {
		var parsed any
		json.Unmarshal(raw, &parsed)
		result["legacy_stat_health"] = parsed
	}

	// Integration v1 WAN interfaces list
	wansRaw, err := s.udr.get("/sites/" + siteID + "/wans")
	if err != nil {
		result["v1_wans_error"] = err.Error()
	} else {
		var parsed any
		json.Unmarshal(wansRaw, &parsed)
		result["v1_wans"] = parsed
	}

	// Legacy stat/device — gateway uplinks (per-WAN port traffic)
	devRaw, err := s.udr.getDeviceStat(siteID)
	if err != nil {
		result["device_error"] = err.Error()
	} else {
		var devResp struct {
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(devRaw, &devResp) == nil {
			for _, dev := range devResp.Data {
				devType, _ := dev["type"].(string)
				if devType == "ugw" || devType == "udm" || devType == "uxg" {
					// Gateway found — extract uplink info
					gwInfo := map[string]any{}
					if uplink, ok := dev["uplink"].(map[string]any); ok {
						gwInfo["uplink"] = uplink
					}
					if uptable, ok := dev["uplink_table"].([]any); ok {
						gwInfo["uplink_table"] = uptable
					}
					if wan1, ok := dev["wan1"].(map[string]any); ok {
						gwInfo["wan1"] = wan1
					}
					if wan2, ok := dev["wan2"].(map[string]any); ok {
						gwInfo["wan2"] = wan2
					}
					gwInfo["type"] = devType
					gwInfo["name"] = dev["name"]
					result["gateway"] = gwInfo
					break
				}
			}
		}
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
	mux.HandleFunc("GET /api/wan/raw", srv.handleWanRaw)
	mux.HandleFunc("POST /api/clients/{clientId}/authorize", srv.handleAuthorize)

	// Static files (embedded, strip "public" prefix)
	staticSub, err := fs.Sub(staticFS, "public")
	if err != nil {
		log.Fatalf("[FATAL] embedded fs: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticSub)))

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

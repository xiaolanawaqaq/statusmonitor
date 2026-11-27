package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed static/*.html
var staticFiles embed.FS

var (
	store     *Store
	cfg       Config
	adminTok  string
	scheduler *Scheduler
	startedAt time.Time
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	flag.Parse()
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if err := cfg.Normalize(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	adminTok = envString(cfg.Admin.Token)
	if adminTok == "" {
		log.Print("admin token is not configured; management APIs are disabled")
	}
	dbPath := cfg.Storage.Path
	if v := os.Getenv("STATUS_DB"); v != "" {
		dbPath = v
	}
	store, err = OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	store.SetProbeTiming(cfg.Probe.IntervalSeconds, cfg.Probe.TimeoutSeconds)
	added, err := store.SeedChannels(cfg.Channels)
	if err != nil {
		log.Fatalf("seed channels: %v", err)
	}
	log.Printf("db ready (%s), seeded %d channels", dbPath, added)
	listenAddr := *addr
	if !flagSet("addr") && cfg.Server.Addr != "" {
		listenAddr = cfg.Server.Addr
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	scheduler = NewScheduler(store, cfg)
	scheduler.Start()
	defer scheduler.Stop()
	startedAt = time.Now()
	server := &http.Server{
		Handler: newHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		WriteTimeout: time.Duration(cfg.Probe.TimeoutSeconds+20) * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	certFile, keyFile := envString(cfg.Server.TLSCert), envString(cfg.Server.TLSKey)
	if certFile != "" {
		go func() { serveErr <- server.ServeTLS(listener, certFile, keyFile) }()
		log.Printf("listening on https://%s", listener.Addr())
		if redirAddr := cfg.Server.HTTPRedirectAddr; redirAddr != "" {
			go serveHTTPRedirect(redirAddr, listenAddr)
		}
	} else {
		go func() { serveErr <- server.Serve(listener) }()
		log.Printf("listening on http://%s", listener.Addr())
	}
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server stopped: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Probe.TimeoutSeconds+12)*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
}

// flagSet reports whether the user explicitly passed -addr.
func flagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// serveHTTPRedirect keeps a plain-HTTP port working by redirecting to the HTTPS address.
func serveHTTPRedirect(addr, httpsAddr string) {
	_, port, err := net.SplitHostPort(httpsAddr)
	if err != nil {
		port = "443"
	}
	proxy := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			target := "https://" + net.JoinHostPort(host, port) + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
		}),
	}
	if err := proxy.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("http redirect listener failed: %v", err)
	}
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/history", handleHistory)
	mux.HandleFunc("/api/dynamics", handleDynamics)
	mux.HandleFunc("/api/dynamics/", handleDynamicByID)
	mux.HandleFunc("/api/incidents", handlePublicIncidents)
	mux.HandleFunc("/api/maintenance", handleMaintenance)
	mux.HandleFunc("/api/health", handleHealth)
	if cfg.API.Enabled {
		pub := newAPIPublic(cfg.API)
		mux.HandleFunc("/v1/", pub.handle)
	}
	mux.HandleFunc("/api/admin/", requireAdmin(handleAdmin))
	mux.HandleFunc("/", handleIndex)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/admin" {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	if err := store.db.PingContext(r.Context()); err != nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	newest, enabled, err := store.ProbeFreshness()
	if err != nil {
		writeErr(w, 503, "database unavailable")
		return
	}
	resp := map[string]any{"ok": true}
	if enabled == 0 {
		writeJSON(w, 200, resp)
		return
	}
	threshold := int64(max(2*cfg.Probe.IntervalSeconds+cfg.Probe.TimeoutSeconds, 60))
	age := time.Now().Unix() - newest
	if newest > 0 {
		resp["last_probe_age_seconds"] = age
	}
	resp["stale_after_seconds"] = threshold
	// 刚启动时给调度器一个完整周期，避免把冷启动误判为探测停摆。
	if !startedAt.IsZero() && time.Since(startedAt) > time.Duration(threshold)*time.Second && (newest == 0 || age > threshold) {
		resp["ok"] = false
		writeJSON(w, 503, resp)
		return
	}
	writeJSON(w, 200, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("storage operation failed: %v", err)
	writeErr(w, 500, "storage operation failed")
}

func onlyMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeErr(w, 405, "method not allowed")
		return false
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		writeErr(w, 400, "invalid JSON or request exceeds 64 KiB")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "request must contain one JSON value")
		return false
	}
	return true
}

func queryInt(w http.ResponseWriter, r *http.Request, key string, def, lo, hi int) (int, bool) {
	text := r.URL.Query().Get(key)
	if text == "" {
		return def, true
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < lo || n > hi {
		writeErr(w, 400, "invalid "+key)
		return 0, false
	}
	return n, true
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	window, ok := queryInt(w, r, "window", 27000, 300, 604800)
	if !ok {
		return
	}
	snap, err := store.Snapshot(window)
	if err != nil {
		serverError(w, err)
		return
	}
	snap.Site = SiteInfo{Title: cfg.Site.Title, Description: cfg.Site.Description}
	if r.URL.Query().Get("format") == "lcapi" {
		writeJSON(w, 200, CompatStatus(snap))
		return
	}
	writeJSON(w, 200, snap)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	days, ok := queryInt(w, r, "days", 30, 1, 90)
	if !ok {
		return
	}
	hist, err := store.History(days)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, hist)
}

func handleDynamics(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	list, err := store.ListDynamics(200)
	if err != nil {
		serverError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "lcapi" {
		writeJSON(w, 200, CompatDynamics(list))
		return
	}
	writeJSON(w, 200, list)
}

func handleDynamicByID(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	d, err := store.GetDynamics(strings.TrimPrefix(r.URL.Path, "/api/dynamics/"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "dynamic not found")
		} else {
			serverError(w, err)
		}
		return
	}
	writeJSON(w, 200, d)
}

func handlePublicIncidents(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	incs, err := store.Incidents(100)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, incs)
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminTok == "" {
			writeErr(w, 503, "admin disabled: configure ADMIN_TOKEN")
			return
		}
		tok := r.Header.Get("X-Admin-Token")
		if tok == "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		given, expected := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(adminTok))
		if subtle.ConstantTimeCompare(given[:], expected[:]) != 1 {
			writeErr(w, 401, "invalid admin token")
			return
		}
		next(w, r)
	}
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "channels":
		adminChannels(w, r)
	case len(parts) == 2 && parts[0] == "channels":
		adminChannelByID(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "probe":
		adminProbeNow(w, r, parts[1])
	case len(parts) >= 1 && parts[0] == "dynamics":
		adminDynamics(w, r, parts)
	case len(parts) >= 1 && parts[0] == "maintenance":
		adminMaintenance(w, r, parts)
	case len(parts) == 1 && parts[0] == "alerts":
		adminAlertReceivers(w, r)
	case len(parts) == 2 && parts[0] == "alerts" && parts[1] == "test":
		adminAlertTest(w, r)
	case len(parts) == 1 && parts[0] == "incidents":
		adminIncidents(w, r)
	case len(parts) == 2 && parts[0] == "incidents" && parts[1] == "resolve":
		adminResolveIncidents(w, r)
	default:
		writeErr(w, 404, "not found")
	}
}

// adminChannel 在渠道配置上附加最近一次探测结果，供后台直接看出配置错误。
type adminChannel struct {
	Channel
	CurrentStatus string `json:"current_status"`
	ConfigError   bool   `json:"config_error"`
	LastError     string `json:"last_error,omitempty"`
}

func adminChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		chs, err := store.ListChannels(false)
		if err != nil {
			serverError(w, err)
			return
		}
		health, err := store.LatestProbeHealth()
		if err != nil {
			serverError(w, err)
			return
		}
		out := make([]adminChannel, 0, len(chs))
		for _, ch := range chs {
			ac := adminChannel{Channel: ch, CurrentStatus: "nodata"}
			if h, ok := health[ch.ID]; ok {
				ac.LastError = h.Error
				ac.ConfigError = h.ConfigError
				switch {
				case h.ConfigError:
					ac.CurrentStatus = "config_error"
				case h.OK:
					ac.CurrentStatus = "ok"
				default:
					ac.CurrentStatus = "down"
				}
			}
			out = append(out, ac)
		}
		writeJSON(w, 200, out)
	case http.MethodPost, http.MethodPut:
		var ch Channel
		if !readJSON(w, r, &ch) {
			return
		}
		if err := validateChannel(ch); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if err := store.UpsertChannel(ch); err != nil {
			serverError(w, err)
			return
		}
		scheduler.Forget(ch.ID)
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func channelByID(w http.ResponseWriter, id string) *Channel {
	chs, err := store.ListChannels(false)
	if err != nil {
		serverError(w, err)
		return nil
	}
	for i := range chs {
		if chs[i].ID == id {
			return &chs[i]
		}
	}
	writeErr(w, 404, "channel not found")
	return nil
}

func adminChannelByID(w http.ResponseWriter, r *http.Request, id string) {
	ch := channelByID(w, id)
	if ch == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, ch)
	case http.MethodDelete:
		if err := store.DeleteChannel(id); err != nil {
			serverError(w, err)
			return
		}
		scheduler.Forget(id)
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func adminProbeNow(w http.ResponseWriter, r *http.Request, id string) {
	if !onlyMethod(w, r, http.MethodPost) {
		return
	}
	ch := channelByID(w, id)
	if ch == nil {
		return
	}
	res, err := scheduler.ProbeNow(r.Context(), *ch)
	if errors.Is(err, ErrProbeBusy) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func adminDynamics(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		list, err := store.ListDynamics(200)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 200, list)
	case len(parts) == 1 && r.Method == http.MethodPost:
		var in DynamicInput
		if !readJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Title) == "" || len(in.Title) > 300 || len(in.Body) > 32000 {
			writeErr(w, 400, "title is required (max 300 bytes); body max 32000 bytes")
			return
		}
		if in.Kind != "" && !validDynamicKind(in.Kind) {
			writeErr(w, 400, "kind must be notice, incident or maintenance")
			return
		}
		if in.State != "" && !validDynamicState(in.State) {
			writeErr(w, 400, "state must be published, active or resolved")
			return
		}
		id, err := store.AddDynamic(in)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true, "id": id})
	case len(parts) == 2 && r.Method == http.MethodGet:
		d, err := store.GetDynamics(parts[1])
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, 404, "dynamic not found")
			} else {
				serverError(w, err)
			}
			return
		}
		writeJSON(w, 200, d)
	case len(parts) == 2 && r.Method == http.MethodPost, len(parts) == 2 && r.Method == http.MethodPatch:
		var patch map[string]any
		if !readJSON(w, r, &patch) {
			return
		}
		if err := store.UpdateDynamic(parts[1], patch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, 404, "dynamic not found")
			} else {
				writeErr(w, 400, err.Error())
			}
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		if err := store.DeleteDynamic(parts[1]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, 404, "dynamic not found")
			} else {
				serverError(w, err)
			}
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(parts) == 3 && parts[2] == "updates" && r.Method == http.MethodPost:
		var in struct {
			Label string `json:"label"`
			Body  string `json:"body"`
			At    int64  `json:"at"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Label) == "" || len(in.Label) > 200 || len(in.Body) > 32000 {
			writeErr(w, 400, "label is required (max 200 bytes); body max 32000 bytes")
			return
		}
		id, err := store.AddDynamicUpdate(parts[1], in.Label, in.Body, in.At)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, 404, "dynamic not found")
			} else {
				serverError(w, err)
			}
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true, "id": id})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func adminAlertReceivers(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, 200, map[string]any{
		"enabled":       cfg.Alerts.Enabled,
		"receivers":     alertReceivers(cfg.Alerts),
		"last_delivery": lastAlertDelivery(),
	})
}

func adminAlertTest(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodPost) {
		return
	}
	if !cfg.Alerts.Enabled {
		writeErr(w, 400, "alerts disabled")
		return
	}
	var in struct {
		Receiver string `json:"receiver"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || (len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &in) != nil) {
		writeErr(w, 400, "invalid test request")
		return
	}
	if in.Receiver != "" {
		found := false
		for _, receiver := range alertReceivers(cfg.Alerts) {
			if receiver.ID == in.Receiver {
				found = true
				if !receiver.Configured {
					writeErr(w, 400, "selected alert receiver is not configured")
					return
				}
			}
		}
		if !found {
			writeErr(w, 400, "unknown alert receiver")
			return
		}
	}
	if !hasAlertReceiver(cfg.Alerts) {
		writeErr(w, 400, "no alert receiver configured")
		return
	}
	ch := Channel{ID: "alert-test", Name: "测试渠道", Group: "告警"}
	results, err := deliverAlert(r.Context(), alertClient(), cfg.Alerts, "test", ch, "告警测试：这是一条测试消息", in.Receiver)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "results": results})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "results": results})
}

func adminIncidents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		incs, err := store.Incidents(100)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 200, incs)
	case http.MethodPost:
		var in struct {
			ChannelID string `json:"channel_id"`
			Reason    string `json:"reason"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 2000 {
			writeErr(w, 400, "reason is required (max 2000 bytes)")
			return
		}
		ch := channelByID(w, in.ChannelID)
		if ch == nil {
			return
		}
		id, err := store.OpenManualIncident(ch.ID, groupOr(ch.Group), in.Reason)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true, "id": id})
	default:
		writeErr(w, 405, "method not allowed")
	}
}

func adminResolveIncidents(w http.ResponseWriter, r *http.Request) {
	if !onlyMethod(w, r, http.MethodPost) {
		return
	}
	var in struct {
		ID int64 `json:"id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.ID < 1 {
		writeErr(w, 400, "incident id is required")
		return
	}
	if err := store.ResolveIncidentByID(in.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, 404, "open incident not found")
		} else {
			serverError(w, err)
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, 405, "method not allowed")
		return
	}
	name := ""
	switch r.URL.Path {
	case "/", "/static/index.html":
		name = "static/index.html"
	case "/admin", "/static/admin.html":
		name = "static/admin.html"
	default:
		http.NotFound(w, r)
		return
	}
	content, err := staticFiles.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

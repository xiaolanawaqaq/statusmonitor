package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 每 IP 令牌桶限流 ----------

type limiterBucket struct {
	tokens float64
	last   time.Time
}

type publicLimiter struct {
	mu      sync.Mutex
	rpm     int
	buckets map[string]*limiterBucket
}

func newPublicLimiter(rpm int) *publicLimiter {
	return &publicLimiter{rpm: rpm, buckets: make(map[string]*limiterBucket)}
}

func (l *publicLimiter) allow(ip string, now time.Time) (ok bool, remaining int, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 4096 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	rate := float64(l.rpm) / 60.0
	b := l.buckets[ip]
	if b == nil {
		b = &limiterBucket{tokens: float64(l.rpm), last: now}
		l.buckets[ip] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rate
		if b.tokens > float64(l.rpm) {
			b.tokens = float64(l.rpm)
		}
		b.last = now
	}
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
		return false, 0, int(wait.Seconds()) + 1
	}
	b.tokens--
	return true, int(b.tokens), 0
}

// ---------- 短 TTL 响应缓存 ----------

type cacheEntry struct {
	body   []byte
	status int
	at     time.Time
}

type publicCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]cacheEntry
}

func newPublicCache(seconds int) *publicCache {
	return &publicCache{ttl: time.Duration(seconds) * time.Second, m: make(map[string]cacheEntry)}
}

func (c *publicCache) get(key string, now time.Time) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.Sub(e.at) > c.ttl {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *publicCache) put(key string, e cacheEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 256 {
		for k, old := range c.m {
			if now.Sub(old.at) > 2*c.ttl {
				delete(c.m, k)
			}
		}
	}
	c.m[key] = e
}

// ---------- 响应捕获 ----------

type captureWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.body = append(w.body, b...)
	return w.ResponseWriter.Write(b)
}

// ---------- /v1/ 公开端点 ----------

type apiPublic struct {
	limiter *publicLimiter
	cache   *publicCache
	cfg     APIConfig
}

func newAPIPublic(cfg APIConfig) *apiPublic {
	return &apiPublic{
		limiter: newPublicLimiter(cfg.RateLimitRPM),
		cache:   newPublicCache(cfg.CacheSeconds),
		cfg:     cfg,
	}
}

func (a *apiPublic) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range a.cfg.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (a *apiPublic) handle(w http.ResponseWriter, r *http.Request) {
	// CORS
	if origin := r.Header.Get("Origin"); a.originAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeErr(w, 405, "method not allowed")
		return
	}

	// 限流
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(a.cfg.RateLimitRPM))
	ok, remaining, retryAfter := a.limiter.allow(ip, time.Now())
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeErr(w, 429, "rate limit exceeded, slow down")
		return
	}

	// 缓存键含查询串；窗口/天数不同的请求分开缓存。
	key := r.Method + " " + r.URL.RequestURI()
	if a.cfg.CacheSeconds > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(a.cfg.CacheSeconds))
		w.Header().Set("X-Cache", "MISS")
		if e, hit := a.cache.get(key, time.Now()); hit {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(a.cfg.CacheSeconds))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(e.status)
			_, _ = w.Write(e.body)
			return
		}
	}

	cap := &captureWriter{ResponseWriter: w}
	a.route(cap, r)

	if a.cfg.CacheSeconds > 0 && cap.status == 200 {
		a.cache.put(key, cacheEntry{body: cap.body, status: cap.status, at: time.Now()}, time.Now())
	}
}

func (a *apiPublic) route(w http.ResponseWriter, r *http.Request) {
	// 把 /v1/xxx 重写为 /api/xxx 后复用同一套处理器；admin 路由不在白名单内。
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/api" + strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case r2.URL.Path == "/api/status":
		handleStatus(w, r2)
	case r2.URL.Path == "/api/history":
		handleHistory(w, r2)
	case r2.URL.Path == "/api/dynamics":
		handleDynamics(w, r2)
	case strings.HasPrefix(r2.URL.Path, "/api/dynamics/"):
		handleDynamicByID(w, r2)
	case r2.URL.Path == "/api/maintenance":
		handleMaintenance(w, r2)
	case r2.URL.Path == "/api/health":
		handleHealth(w, r2)
	default:
		writeErr(w, 404, "unknown public endpoint; see /v1/status, /v1/history, /v1/dynamics, /v1/maintenance, /v1/health")
	}
}

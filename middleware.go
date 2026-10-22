package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": code})
}

// decodeJSON reads a small JSON body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

// loggingMiddleware logs every request: method, path, status, duration, remote addr.
// Headers (cookies, device key) and bodies are never logged.
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

// corsMiddleware allows the configured frontend origin and handles preflight.
func corsMiddleware(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// deviceAuthMiddleware requires the X-Device-Key header on /api/device/* when key is set.
func deviceAuthMiddleware(logger *slog.Logger, key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" && strings.HasPrefix(r.URL.Path, "/api/device/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Device-Key")), []byte(key)) != 1 {
			logger.Info("device auth failed", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfMiddleware: every state-changing request under /api/ (except the
// device endpoints, which use X-Device-Key) must come from the frontend origin
// with a JSON content type. Browsers can't send that cross-site without CORS.
func csrfMiddleware(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/device/") {
				mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if r.Header.Get("Origin") != origin || mt != "application/json" {
					writeErr(w, http.StatusForbidden, "forbidden")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the first X-Forwarded-For value; only Caddy can reach this
// container so the header is trusted. Falls back to the socket address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is an in-memory token bucket per key.
type rateLimiter struct {
	mu      sync.Mutex
	perMin  float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: float64(perMin), buckets: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 10000 { // drop idle buckets (full again anyway)
			for k, o := range l.buckets {
				if now.Sub(o.last) > time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.perMin, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * l.perMin
	if b.tokens > l.perMin {
		b.tokens = l.perMin
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *rateLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next(w, r)
	}
}

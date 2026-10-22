package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(map[string]string{"DATABASE_URL": "x", "GOOGLE_CLIENT_ID": "g"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Prices["tea"] != 5 || c.Prices["coffee"] != 10 || c.Prices["hotwater"] != 0 {
		t.Fatalf("prices %v", c.Prices)
	}
	if !c.RefundOnTimeout || c.SessionDays != 30 || len(c.Packages) != 3 || c.Packages[1] != (rechargePackage{100, 100}) {
		t.Fatalf("defaults %+v", c)
	}
}

func TestLoadConfigOverridesAndErrors(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "x", "GOOGLE_CLIENT_ID": "g"}
	with := func(k, v string) map[string]string {
		m := map[string]string{k: v}
		for a, b := range base {
			m[a] = b
		}
		return m
	}
	c, err := loadConfig(env(with("PRICE_TEA", " 7 ")))
	if err != nil || c.Prices["tea"] != 7 {
		t.Fatalf("%v %v", c, err)
	}
	if c, _ = loadConfig(env(with("REFUND_ON_TIMEOUT", "false"))); c.RefundOnTimeout {
		t.Fatal("refund should be off")
	}
	for k, v := range map[string]string{"PRICE_COFFEE": "-1", "PRICE_TEA": "abc", "SESSION_DAYS": "0", "RECHARGE_PACKAGES": "50,x"} {
		if _, err := loadConfig(env(with(k, v))); err == nil {
			t.Errorf("%s=%s should fail", k, v)
		}
	}
	if _, err := loadConfig(env(nil)); err == nil {
		t.Error("missing required env should fail")
	}
}

func TestParseAdminEmails(t *testing.T) {
	m := parseAdminEmails(" Boss@Example.com , ,b@x.io")
	if len(m) != 2 || !m["boss@example.com"] || !m["b@x.io"] {
		t.Fatalf("%v", m)
	}
}

func TestCSRF(t *testing.T) {
	h := csrfMiddleware("https://site.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(method, path, origin, ct string) int {
		r := httptest.NewRequest(method, path, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		method, path, origin, ct string
		want                     int
	}{
		{"POST", "/api/unlock", "https://site.test", "application/json", 204},
		{"POST", "/api/unlock", "https://site.test", "application/json; charset=utf-8", 204},
		{"POST", "/api/unlock", "https://evil.test", "application/json", 403},
		{"POST", "/api/unlock", "", "application/json", 403},
		{"POST", "/api/unlock", "https://site.test", "text/plain", 403},
		{"POST", "/api/unlock", "https://site.test", "", 403},
		{"GET", "/api/me", "", "", 204},
		{"POST", "/api/device/consumed", "", "", 204},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.origin, c.ct); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(10)
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		if !l.allow("a", t0) {
			t.Fatalf("request %d denied", i)
		}
	}
	if l.allow("a", t0) {
		t.Fatal("11th should be denied")
	}
	if !l.allow("b", t0) {
		t.Fatal("other key independent")
	}
	if !l.allow("a", t0.Add(7*time.Second)) { // 10/min refills ~1 per 6s
		t.Fatal("should refill")
	}
	if l.allow("a", t0.Add(7*time.Second)) {
		t.Fatal("only one token refilled")
	}
}

func TestRateLimitMiddlewareUsesForwardedFor(t *testing.T) {
	l := newRateLimiter(1)
	h := l.wrap(func(w http.ResponseWriter, r *http.Request) {})
	do := func(xff string) int {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	if do("1.1.1.1, 10.0.0.1") != 200 || do("1.1.1.1, 9.9.9.9") != 429 || do("2.2.2.2") != 200 {
		t.Fatal("limiter should key on first XFF value")
	}
}

func TestSessionCookieFlags(t *testing.T) {
	w := httptest.NewRecorder()
	http.SetCookie(w, sessionCookie("tok", 30))
	c := w.Result().Cookies()[0]
	if c.Name != "tea_session" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 30*86400 {
		t.Fatalf("%+v", c)
	}
	if sessionCookie("", 0).MaxAge >= 0 {
		t.Fatal("expiry cookie must have negative MaxAge")
	}
}

func TestTokenHash(t *testing.T) {
	a, _ := newToken()
	b, _ := newToken()
	if a == b || len(a) != 43 || strings.ContainsAny(a, "+/=") {
		t.Fatalf("bad token %q", a)
	}
	if len(hashToken(a)) != 32 {
		t.Fatal("sha256 length")
	}
}

func TestMachineReserve(t *testing.T) {
	s := &machineState{}
	now := time.Now()
	if s.reserve(now) != "offline" {
		t.Fatal("offline expected")
	}
	s.lastSeen, s.devState, s.devAt = now, "ready", now
	if s.reserve(now) != "" || s.reserve(now) != "busy" {
		t.Fatal("second reserve must be busy")
	}
	s.release()
	if s.reserve(now) != "" {
		t.Fatal("reserve after release")
	}
	s.queue(now, command{ID: 7, Drink: "tea", Seconds: 25})
	if s.reserve(now) != "busy" {
		t.Fatal("pending command keeps machine busy")
	}
	if c, _ := s.poll(now); c == nil || c.ID != 7 {
		t.Fatal("poll should deliver order id as command id")
	}
}

//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testOrigin = "https://site.test"

// startPostgres runs a throwaway postgres:16-alpine and returns its URL.
func startPostgres(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm", "-e", "POSTGRES_PASSWORD=pw", "-e", "POSTGRES_DB=tea",
		"-p", "127.0.0.1::5432", "postgres:16-alpine").CombinedOutput()
	if err != nil {
		t.Skipf("docker unavailable: %v %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })
	portOut, err := exec.Command("docker", "port", id, "5432/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(portOut)), "\n")
	port := line[strings.LastIndex(line, ":")+1:]
	return fmt.Sprintf("postgres://postgres:pw@127.0.0.1:%s/tea?sslmode=disable", port)
}

type env2 struct {
	srv *server
	h   http.Handler
	db  *pgxpool.Pool
}

func setup(t *testing.T) *env2 {
	t.Helper()
	url := startPostgres(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()
	db, err := connectDB(ctx, url, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := migrate(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, logger); err != nil { // idempotent
		t.Fatal(err)
	}
	unlockSeconds = 25
	cfg, err := loadConfig(func(k string) string {
		return map[string]string{"DATABASE_URL": url, "GOOGLE_CLIENT_ID": "g", "FRONTEND_ORIGIN": testOrigin, "ADMIN_EMAILS": "Admin@x.io"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(cfg, db, logger)
	now := time.Now()
	srv.state.lastSeen, srv.state.devState, srv.state.devAt = now, "ready", now
	return &env2{srv, srv.handler(), db}
}

// mkUser inserts a user with credits and returns id and a session token.
func (e *env2) mkUser(t *testing.T, email string, credits int) (int64, string) {
	t.Helper()
	var id int64
	err := e.db.QueryRow(context.Background(), `INSERT INTO users(google_sub,email,credits,is_admin) VALUES ($1,$1,$2,$3) RETURNING id`,
		email, credits, e.srv.cfg.AdminEmails[email]).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.srv.createSession(context.Background(), id, "test", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	return id, tok
}

func (e *env2) do(method, path, tok string, body any) (int, map[string]any) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e *env2) balance(t *testing.T, id int64) int {
	var c int
	if err := e.db.QueryRow(context.Background(), `SELECT credits FROM users WHERE id=$1`, id).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *env2) count(t *testing.T, q string, args ...any) int {
	var n int
	if err := e.db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPurchaseRefundAdmin(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, tok := e.mkUser(t, "u@x.io", 12)

	// Purchase: tea costs 5.
	code, res := e.do("POST", "/api/unlock", tok, map[string]any{"drink": "tea"})
	if code != 200 || res["credits"] != float64(7) {
		t.Fatalf("unlock: %d %v", code, res)
	}
	orderID := int64(res["orderId"].(float64))
	if e.balance(t, uid) != 7 {
		t.Fatal("balance not deducted")
	}
	var amount, after int
	if err := e.db.QueryRow(ctx, `SELECT amount,balance_after FROM credit_transactions WHERE order_id=$1 AND kind='purchase'`, orderID).Scan(&amount, &after); err != nil || amount != -5 || after != 7 {
		t.Fatalf("ledger: %v %d %d", err, amount, after)
	}
	// Machine is now busy for a second request.
	if code, _ := e.do("POST", "/api/unlock", tok, map[string]any{"drink": "tea"}); code != 409 {
		t.Fatalf("expected busy 409, got %d", code)
	}
	// Device receives the order id as command id.
	if c, _ := e.srv.state.poll(time.Now()); c == nil || c.ID != orderID {
		t.Fatal("command id should equal order id")
	}

	// Refund twice -> restored exactly once.
	for i, want := range []bool{true, false} {
		got, err := e.srv.refundOrder(ctx, orderID, "timeout")
		if err != nil || got != want {
			t.Fatalf("refund #%d: %v %v", i, got, err)
		}
	}
	if e.balance(t, uid) != 12 {
		t.Fatalf("balance after refund = %d", e.balance(t, uid))
	}
	if n := e.count(t, `SELECT count(*) FROM credit_transactions WHERE order_id=$1 AND kind='refund'`, orderID); n != 1 {
		t.Fatalf("refund ledger rows = %d", n)
	}
	// A late "consumed" must not undo the refund.
	if done, _ := e.srv.completeOrder(ctx, orderID); done {
		t.Fatal("refunded order must not complete")
	}

	// Insufficient credits -> 402, no ledger row, no order, machine released.
	e.srv.state.mu.Lock()
	e.srv.state.busyUntil = time.Time{}
	e.srv.state.mu.Unlock()
	_, tok2 := e.mkUser(t, "poor@x.io", 3)
	before := e.count(t, `SELECT count(*) FROM credit_transactions`)
	code, res = e.do("POST", "/api/unlock", tok2, map[string]any{"drink": "coffee"})
	if code != 402 || res["error"] != "insufficient_credits" || res["credits"] != float64(3) || res["price"] != float64(10) {
		t.Fatalf("expected 402, got %d %v", code, res)
	}
	if e.count(t, `SELECT count(*) FROM credit_transactions`) != before || e.count(t, `SELECT count(*) FROM orders`) != 1 {
		t.Fatal("402 must leave no ledger row or order")
	}
	// Free hot water works at 0 credits and writes no ledger row.
	_, tok0 := e.mkUser(t, "zero@x.io", 0)
	if code, res = e.do("POST", "/api/unlock", tok0, map[string]any{"drink": "hotwater"}); code != 200 {
		t.Fatalf("hotwater: %d %v", code, res)
	}
	if e.count(t, `SELECT count(*) FROM credit_transactions`) != before {
		t.Fatal("free drink must not write ledger")
	}

	// Unauthenticated unlock.
	if code, _ := e.do("POST", "/api/unlock", "", map[string]any{"drink": "tea"}); code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}

	// Admin adjust: non-admin 403, cannot go negative, grants work, ledger balance matches.
	if code, _ := e.do("POST", "/api/admin/credits", tok, map[string]any{"email": "u@x.io", "amount": 5, "note": "x"}); code != 403 {
		t.Fatalf("non-admin: %d", code)
	}
	_, adminTok := e.mkUser(t, "admin@x.io", 0)
	adj := func(amount int, note string) (int, map[string]any) {
		return e.do("POST", "/api/admin/credits", adminTok, map[string]any{"email": "U@x.io", "amount": amount, "note": note})
	}
	if code, res = adj(-13, "too much"); code != 409 {
		t.Fatalf("negative: %d %v", code, res)
	}
	if e.balance(t, uid) != 12 {
		t.Fatal("failed adjust must not change balance")
	}
	if code, res = adj(8, "gift"); code != 200 || res["credits"] != float64(20) {
		t.Fatalf("grant: %d %v", code, res)
	}
	if code, _ = adj(5, ""); code != 400 {
		t.Fatalf("note required: %d", code)
	}
	if code, _ = adj(10001, "big"); code != 400 {
		t.Fatalf("limit: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM credit_transactions WHERE kind='admin_adjust' AND balance_after=20 AND amount=8 AND admin_user_id IS NOT NULL`); n != 1 {
		t.Fatal("admin ledger row missing")
	}
	// Admin cannot disable self; can disable others, who are then locked out.
	if code, _ = e.do("POST", "/api/admin/users/disable", adminTok, map[string]any{"email": "admin@x.io", "disabled": true}); code != 400 {
		t.Fatalf("self disable: %d", code)
	}
	if code, _ = e.do("POST", "/api/admin/users/disable", adminTok, map[string]any{"email": "u@x.io", "disabled": true}); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ = e.do("GET", "/api/me", tok, nil); code != 403 {
		t.Fatalf("disabled user: %d", code)
	}
	// Admin listings.
	if code, _ = e.do("GET", "/api/admin/orders?status=refunded", adminTok, nil); code != 200 {
		t.Fatalf("admin orders: %d", code)
	}
	// Recharge is a stub.
	_, tok3 := e.mkUser(t, "r@x.io", 0)
	if code, res = e.do("POST", "/api/recharge/checkout", tok3, map[string]any{"credits": 100}); code != 501 {
		t.Fatalf("recharge: %d", code)
	}
	if code, _ = e.do("POST", "/api/recharge/checkout", tok3, map[string]any{"credits": 7}); code != 400 {
		t.Fatalf("bad package: %d", code)
	}
}

func TestSweepAndLogin(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, _ := e.mkUser(t, "s@x.io", 10)
	// Simulate a crash: a purchased, never-resolved order older than the window.
	orderID, _, err := e.srv.purchase(ctx, uid, "coffee", 10)
	if err != nil || e.balance(t, uid) != 0 {
		t.Fatal(err)
	}
	e.db.Exec(ctx, `UPDATE orders SET created_at = now() - interval '10 minutes' WHERE id=$1`, orderID)
	e.srv.sweepPending(ctx)
	if e.balance(t, uid) != 10 {
		t.Fatal("startup sweep should refund")
	}

	// Google login (verifier stubbed): creates user, admin flag from ADMIN_EMAILS, sets cookie.
	e.srv.verifyGoogle = func(ctx context.Context, c string) (*googleIdentity, error) {
		return &googleIdentity{Sub: "sub1", Email: "admin@x.io", Name: "Ad"}, nil
	}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]string{"credential": "whatever"})
	r := httptest.NewRequest("POST", "/api/auth/google", &buf)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	cs := w.Result().Cookies()
	if w.Code != 200 || len(cs) != 1 || !cs[0].Secure || !cs[0].HttpOnly {
		t.Fatalf("login: %d %v", w.Code, w.Body.String())
	}
	if code, res := e.do("GET", "/api/me", cs[0].Value, nil); code != 200 || res["isAdmin"] != true {
		t.Fatalf("me: %d %v", code, res)
	}
	if code, _ := e.do("POST", "/api/auth/logout", cs[0].Value, map[string]any{}); code != 200 {
		t.Fatal("logout")
	}
	if code, _ := e.do("GET", "/api/me", cs[0].Value, nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

// Command teamachine is the backend for a coffee machine unlock system.
// A website calls POST /api/unlock to queue a drink command; an ESP32
// long-polls GET /api/device/poll to receive it.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const onlineThreshold = 60 * time.Second

var validDrinks = map[string]bool{"coffee": true, "tea": true, "hotwater": true}

type command struct {
	ID      int64  `json:"id"`
	Drink   string `json:"drink"`
	Seconds int    `json:"seconds"`
}

type machineState struct {
	mu sync.Mutex

	pending   *command
	nextID    int64
	lastSeen  time.Time
	busyUntil time.Time
	busyDrink string

	online  bool
	waiters []chan command
}

func (s *machineState) isOnlineLocked(now time.Time) bool {
	return !s.lastSeen.IsZero() && now.Sub(s.lastSeen) <= onlineThreshold
}

// monitorOnline periodically checks for online<->offline transitions caused
// purely by time passing (no incoming poll), and logs them.
func (s *machineState) monitorOnline(logger *slog.Logger) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		s.mu.Lock()
		online := s.isOnlineLocked(now)
		changed := online != s.online
		s.online = online
		s.mu.Unlock()
		if changed {
			if online {
				logger.Info("device online")
			} else {
				logger.Info("device offline")
			}
		}
	}
}

func (s *machineState) status(now time.Time) (online, busy bool, drink string, secondsLeft int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	online = s.isOnlineLocked(now)
	busy = s.pending != nil || now.Before(s.busyUntil)
	if busy {
		drink = s.busyDrink
		if now.Before(s.busyUntil) {
			secondsLeft = int(s.busyUntil.Sub(now).Seconds()) + 1
		}
		if s.pending != nil {
			drink = s.pending.Drink
		}
	}
	return
}

// unlock queues a command for the device. Returns ("", cmd) on success, or
// (errCode, zero) on failure. errCode is one of "offline", "busy".
func (s *machineState) unlock(now time.Time, drink string, seconds int) (errCode string, cmd command) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isOnlineLocked(now) {
		return "offline", command{}
	}
	if s.pending != nil || now.Before(s.busyUntil) {
		return "busy", command{}
	}

	s.nextID++
	cmd = command{ID: s.nextID, Drink: drink, Seconds: seconds}

	if len(s.waiters) > 0 {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.busyUntil = now.Add(time.Duration(seconds) * time.Second)
		s.busyDrink = drink
		ch <- cmd
	} else {
		s.pending = &cmd
	}
	return "", cmd
}

// poll registers the device as seen and either returns a ready command
// immediately or a channel to wait on.
func (s *machineState) poll(now time.Time) (cmd *command, wait chan command) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastSeen = now
	s.online = true

	if s.pending != nil {
		c := *s.pending
		s.pending = nil
		s.busyUntil = now.Add(time.Duration(c.Seconds) * time.Second)
		s.busyDrink = c.Drink
		return &c, nil
	}

	ch := make(chan command, 1)
	s.waiters = append(s.waiters, ch)
	return nil, ch
}

func (s *machineState) removeWaiter(ch chan command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.waiters {
		if w == ch {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			break
		}
	}
}

type server struct {
	state  *machineState
	logger *slog.Logger
}

func (srv *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (srv *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	online, busy, drink, secondsLeft := srv.state.status(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"online":      online,
		"busy":        busy,
		"drink":       drink,
		"secondsLeft": secondsLeft,
	})
}

func (srv *server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Drink string `json:"drink"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validDrinks[body.Drink] {
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid drink"})
		return
	}

	seconds := unlockSeconds
	errCode, cmd := srv.state.unlock(time.Now(), body.Drink, seconds)
	switch errCode {
	case "offline":
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "offline")
		writeJSON(w, http.StatusConflict, map[string]any{"error": "machine offline"})
	case "busy":
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "busy")
		writeJSON(w, http.StatusConflict, map[string]any{"error": "machine busy"})
	default:
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "ok")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "drink": cmd.Drink, "seconds": cmd.Seconds})
	}
}

func (srv *server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	wait := 25
	if v := r.URL.Query().Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			wait = n
		}
	}

	cmd, ch := srv.state.poll(time.Now())
	if cmd != nil {
		srv.logger.Info("device poll", "result", "command", "drink", cmd.Drink)
		writeJSON(w, http.StatusOK, cmd)
		return
	}

	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()

	select {
	case c := <-ch:
		srv.logger.Info("device poll", "result", "command", "drink", c.Drink)
		writeJSON(w, http.StatusOK, c)
	case <-timer.C:
		srv.state.removeWaiter(ch)
		srv.logger.Info("device poll", "result", "no_command")
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
		srv.state.removeWaiter(ch)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// loggingMiddleware logs every request: method, path, status, duration, remote addr.
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

var unlockSeconds int

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	port := getEnv("PORT", "8080")
	unlockSecondsStr := getEnv("UNLOCK_SECONDS", "25")
	frontendOrigin := getEnv("FRONTEND_ORIGIN", "*")

	var err error
	unlockSeconds, err = strconv.Atoi(unlockSecondsStr)
	if err != nil || unlockSeconds <= 0 {
		logger.Error("invalid UNLOCK_SECONDS, using default", "value", unlockSecondsStr)
		unlockSeconds = 25
	}

	state := &machineState{}
	srv := &server{state: state, logger: logger}
	go state.monitorOnline(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /api/status", srv.handleStatus)
	mux.HandleFunc("POST /api/unlock", srv.handleUnlock)
	mux.HandleFunc("GET /api/device/poll", srv.handleDevicePoll)

	var handler http.Handler = mux
	handler = corsMiddleware(frontendOrigin, handler)
	handler = loggingMiddleware(logger, handler)

	logger.Info("server starting",
		"port", port,
		"unlock_seconds", unlockSeconds,
		"frontend_origin", frontendOrigin,
	)

	addr := fmt.Sprintf(":%s", port)
	if err := http.ListenAndServe(addr, handler); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

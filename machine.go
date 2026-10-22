package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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
	busyID    int64

	online  bool
	waiters []chan command

	devState string // last reported by /api/device/status
	devVolts float64
	devAt    time.Time
}

var validMachineStates = map[string]bool{"ready": true, "heating": true, "sensor_fault": true}

var (
	statusStale = 30 * time.Second
	tempTable   []tempPoint
)

type tempPoint struct{ volts, celsius float64 }

// parseTempTable parses "volts:celsius,volts:celsius" into points sorted by volts.
func parseTempTable(s string) ([]tempPoint, error) {
	var pts []tempPoint
	for _, pair := range strings.Split(s, ",") {
		v, c, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok {
			return nil, fmt.Errorf("bad pair %q", pair)
		}
		fv, err1 := strconv.ParseFloat(v, 64)
		fc, err2 := strconv.ParseFloat(c, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad pair %q", pair)
		}
		pts = append(pts, tempPoint{fv, fc})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].volts < pts[j].volts })
	return pts, nil
}

// tempFor interpolates the table; below/above are set when volts is outside it.
func tempFor(volts float64) (c float64, below, above bool) {
	t := tempTable
	if volts < t[0].volts {
		return t[0].celsius, true, false
	}
	if volts > t[len(t)-1].volts {
		return t[len(t)-1].celsius, false, true
	}
	for i := 1; i < len(t); i++ {
		if volts <= t[i].volts {
			a, b := t[i-1], t[i]
			if b.volts == a.volts {
				return b.celsius, false, false
			}
			return a.celsius + (b.celsius-a.celsius)*(volts-a.volts)/(b.volts-a.volts), false, false
		}
	}
	return t[len(t)-1].celsius, false, false
}

// machineStateLocked returns the effective heating state: "unknown" when no
// status was received recently.
func (s *machineState) machineStateLocked(now time.Time) string {
	if s.devState == "" || now.Sub(s.devAt) > statusStale {
		return "unknown"
	}
	return s.devState
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
	if s.machineStateLocked(now) != "ready" {
		return "not_ready", command{}
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
		s.busyID = cmd.ID
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
		s.busyID = c.ID
		return &c, nil
	}

	ch := make(chan command, 1)
	s.waiters = append(s.waiters, ch)
	return nil, ch
}

// consumed ends the busy window early if id+drink match the active command
// (pending or in-progress). Idempotent: a stale/unknown id is a no-op.
func (s *machineState) consumed(now time.Time, id int64, drink string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending != nil && s.pending.ID == id && s.pending.Drink == drink {
		s.pending = nil
		return true
	}
	if s.busyID == id && s.busyDrink == drink && now.Before(s.busyUntil) {
		s.busyUntil = now
		return true
	}
	return false
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
	now := time.Now()
	online, busy, drink, secondsLeft := srv.state.status(now)
	srv.state.mu.Lock()
	ms := srv.state.machineStateLocked(now)
	volts := srv.state.devVolts
	srv.state.mu.Unlock()
	var thsVolts, tempC any
	var below, above bool
	if ms != "unknown" {
		thsVolts = volts
		var c float64
		c, below, above = tempFor(volts)
		tempC = c
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"online":       online,
		"busy":         busy,
		"drink":        drink,
		"secondsLeft":  secondsLeft,
		"machineState": ms,
		"thsVolts":     thsVolts,
		"tempC":        tempC,
		"tempBelow":    below,
		"tempAbove":    above,
	})
}

func (srv *server) handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ThsVolts float64 `json:"thsVolts"`
		State    string  `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validMachineStates[body.State] || body.ThsVolts < 0 || body.ThsVolts > 6 {
		srv.logger.Info("device status", "result", "bad_request", "state", body.State, "thsVolts", body.ThsVolts)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "need state ready|heating|sensor_fault and thsVolts 0..6"})
		return
	}
	now := time.Now()
	srv.state.mu.Lock()
	srv.state.devState, srv.state.devVolts, srv.state.devAt = body.State, body.ThsVolts, now
	srv.state.lastSeen = now
	srv.state.online = true
	srv.state.mu.Unlock()
	srv.logger.Info("device status", "state", body.State, "thsVolts", body.ThsVolts)
	w.WriteHeader(http.StatusNoContent)
}

func (srv *server) handleDeviceRejected(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     flexibleID `json:"id"`
		Drink  string     `json:"drink"`
		Reason string     `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid request: %v", err)})
		return
	}
	cleared := srv.state.consumed(time.Now(), int64(body.ID), body.Drink)
	srv.logger.Info("device rejected", "id", int64(body.ID), "drink", body.Drink, "reason", body.Reason, "cleared", cleared)
	w.WriteHeader(http.StatusNoContent)
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
	case "not_ready":
		srv.state.mu.Lock()
		ms := srv.state.machineStateLocked(time.Now())
		srv.state.mu.Unlock()
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "not_ready", "machineState", ms)
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_ready", "machineState": ms})
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

// flexibleID unmarshals a command id given as either a JSON number or a
// numeric string, since callers report it back after receiving it as a number.
type flexibleID int64

func (id *flexibleID) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*id = flexibleID(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("id must be a number or numeric string")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("id must be a number or numeric string")
	}
	*id = flexibleID(n)
	return nil
}

func (srv *server) handleDeviceConsumed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID    flexibleID `json:"id"`
		Drink string     `json:"drink"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reason := fmt.Sprintf("invalid request: %v", err)
		srv.logger.Info("device consumed", "result", "rejected", "reason", reason)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}
	if !validDrinks[body.Drink] {
		reason := fmt.Sprintf("invalid request: drink must be one of coffee, tea, hotwater, got %q", body.Drink)
		srv.logger.Info("device consumed", "result", "rejected", "reason", reason)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": reason})
		return
	}

	ended := srv.state.consumed(time.Now(), int64(body.ID), body.Drink)
	srv.logger.Info("device consumed", "id", int64(body.ID), "drink", body.Drink, "ended", ended)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	deviceKey := os.Getenv("DEVICE_KEY")
	if deviceKey == "" {
		logger.Warn("DEVICE_KEY not set; device endpoints are unauthenticated")
	}

	var err error
	unlockSeconds, err = strconv.Atoi(unlockSecondsStr)
	if err != nil || unlockSeconds <= 0 {
		logger.Error("invalid UNLOCK_SECONDS, using default", "value", unlockSecondsStr)
		unlockSeconds = 25
	}

	if n, err := strconv.Atoi(getEnv("STATUS_STALE_SECONDS", "30")); err == nil && n > 0 {
		statusStale = time.Duration(n) * time.Second
	} else {
		logger.Error("invalid STATUS_STALE_SECONDS, using default 30")
	}
	tempTable, err = parseTempTable(getEnv("TEMP_TABLE", "4.10:78,4.50:88,4.53:89"))
	if err != nil {
		logger.Error("invalid TEMP_TABLE, using default", "error", err)
		tempTable, _ = parseTempTable("4.10:78,4.50:88,4.53:89")
	}
	logger.Info("temp table", "points", tempTable, "status_stale", statusStale.String())

	state := &machineState{}
	srv := &server{state: state, logger: logger}
	go state.monitorOnline(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /api/status", srv.handleStatus)
	mux.HandleFunc("POST /api/unlock", srv.handleUnlock)
	mux.HandleFunc("GET /api/device/poll", srv.handleDevicePoll)
	mux.HandleFunc("POST /api/device/consumed", srv.handleDeviceConsumed)
	mux.HandleFunc("POST /api/device/status", srv.handleDeviceStatus)
	mux.HandleFunc("POST /api/device/rejected", srv.handleDeviceRejected)

	var handler http.Handler = deviceAuthMiddleware(logger, deviceKey, mux)
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

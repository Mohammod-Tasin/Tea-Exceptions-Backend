package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type server struct {
	state  *machineState
	logger *slog.Logger
	cfg    *config
	db     *pgxpool.Pool

	verifyGoogle func(ctx context.Context, credential string) (*googleIdentity, error)

	authLimit, unlockLimit, adminLimit *rateLimiter
}

func newServer(cfg *config, db *pgxpool.Pool, logger *slog.Logger) *server {
	return &server{
		state: &machineState{}, logger: logger, cfg: cfg, db: db,
		verifyGoogle: verifyGoogleToken(cfg.GoogleClientID),
		authLimit:    newRateLimiter(10),
		unlockLimit:  newRateLimiter(20),
		adminLimit:   newRateLimiter(60),
	}
}

func (srv *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /api/config", srv.handleConfig)
	mux.HandleFunc("GET /api/status", srv.handleStatus)

	mux.HandleFunc("POST /api/auth/google", srv.authLimit.wrap(srv.handleGoogleLogin))
	mux.HandleFunc("POST /api/auth/logout", srv.handleLogout)
	mux.HandleFunc("GET /api/me", srv.auth(srv.handleMe))
	mux.HandleFunc("GET /api/me/orders", srv.auth(srv.handleMyOrders))
	mux.HandleFunc("GET /api/me/transactions", srv.auth(srv.handleMyTransactions))

	mux.HandleFunc("POST /api/unlock", srv.unlockLimit.wrap(srv.auth(srv.handleUnlock)))
	mux.HandleFunc("POST /api/recharge/checkout", srv.auth(srv.handleRecharge))

	mux.HandleFunc("GET /api/admin/users", srv.adminOnly(srv.handleAdminUsers))
	mux.HandleFunc("POST /api/admin/credits", srv.adminOnly(srv.handleAdminCredits))
	mux.HandleFunc("POST /api/admin/users/disable", srv.adminOnly(srv.handleAdminDisable))
	mux.HandleFunc("GET /api/admin/transactions", srv.adminOnly(srv.handleAdminTransactions))
	mux.HandleFunc("GET /api/admin/orders", srv.adminOnly(srv.handleAdminOrders))

	mux.HandleFunc("GET /api/device/poll", srv.handleDevicePoll)
	mux.HandleFunc("POST /api/device/consumed", srv.handleDeviceConsumed)
	mux.HandleFunc("POST /api/device/status", srv.handleDeviceStatus)
	mux.HandleFunc("POST /api/device/rejected", srv.handleDeviceRejected)

	var h http.Handler = csrfMiddleware(srv.cfg.FrontendOrigin, mux)
	h = deviceAuthMiddleware(srv.logger, srv.cfg.DeviceKey, h)
	h = corsMiddleware(srv.cfg.FrontendOrigin, h)
	return loggingMiddleware(srv.logger, h)
}

// background runs periodic housekeeping until ctx is cancelled.
func (srv *server) background(ctx context.Context) {
	sess := time.NewTicker(time.Hour)
	sweep := time.NewTicker(30 * time.Second)
	defer sess.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.C:
			srv.deleteExpiredSessions(ctx)
		case <-sweep.C:
			srv.sweepPending(ctx)
		}
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	fatal := func(msg string, err error) {
		logger.Error(msg, "error", err.Error())
		os.Exit(1)
	}

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fatal("invalid configuration", err)
	}
	if cfg.DeviceKey == "" {
		logger.Warn("DEVICE_KEY not set; device endpoints are unauthenticated")
	}

	unlockSeconds, err = strconv.Atoi(getEnv("UNLOCK_SECONDS", "25"))
	if err != nil || unlockSeconds <= 0 {
		logger.Error("invalid UNLOCK_SECONDS, using default", "value", os.Getenv("UNLOCK_SECONDS"))
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	db, err := connectDB(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		fatal("database connection failed", err)
	}
	defer db.Close()
	if err := migrate(ctx, db, logger); err != nil {
		fatal("migration failed", err)
	}

	srv := newServer(cfg, db, logger)
	srv.sweepPending(ctx)
	go srv.state.monitorOnline(logger)
	go srv.background(ctx)

	logger.Info("server starting",
		"port", cfg.Port,
		"unlock_seconds", unlockSeconds,
		"frontend_origin", cfg.FrontendOrigin,
	)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: device long-poll holds responses open ~25s.
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errc:
		fatal("server failed", err)
	case <-ctx.Done():
		logger.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			logger.Error("shutdown error", "error", err)
		}
	}
}

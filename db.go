package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const dbTimeout = 5 * time.Second

func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, dbTimeout)
}

// connectDB retries for up to 60s (the db container may still be starting).
func connectDB(ctx context.Context, url string, logger *slog.Logger) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		pool, err := pgxpool.New(c, url)
		if err == nil {
			if err = pool.Ping(c); err == nil {
				cancel()
				return pool, nil
			}
			pool.Close()
		}
		cancel()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("database unavailable: %w", err)
		}
		logger.Warn("database not ready, retrying", "error", err.Error())
		time.Sleep(2 * time.Second)
	}
}

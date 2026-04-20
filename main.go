// Package main is the entry point for the DNSFox v2 background workers process.
//
// # DB sharing decision: Option A
//
// We import github.com/itsBOTzilla/dnsfoxv2-api as a Go module via a local
// replace directive so the DB pool, Redis client, and repository types are
// shared without duplication.  The workers only import internal/db and
// internal/redis — they never import HTTP handlers — so there is no circular
// dependency risk.
//
// Workers started:
//   - provisioning: polls provisioning_jobs every 10s, dispatches to Warden gRPC
//   - health: probes all active sites every 60s via HTTP
//   - backup: checks for due backups every 5m, runs retention prune every 24h
//   - malware: staggered scans across a 60-minute window
//   - billing: polls stripe_events every 30s, dunning escalation every 15m
//   - cleanup: runs at 2 AM UTC for housekeeping (pins, old jobs, disk warnings)
//   - metrics: hourly aggregation + daily rollup at midnight UTC
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/backup"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/billing"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/cleanup"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/health"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/malware"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/metrics"
	"github.com/itsBOTzilla/dnsfoxv2-workers/workers/provisioning"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ---- Database -----------------------------------------------------------
	dbURL := mustEnv("DATABASE_URL")
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("workers: connect database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("workers: ping database: %v", err)
	}
	defer pool.Close()
	log.Printf("workers: database connected")

	// ---- Redis --------------------------------------------------------------
	redisURL := getEnv("REDIS_URL", "redis://localhost:6379")
	rdbOpts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("workers: parse REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(rdbOpts)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("workers: ping redis: %v", err)
	}
	defer rdb.Close() //nolint:errcheck
	log.Printf("workers: redis connected")

	// ---- Payload encryption master key -------------------------------------
	// The provisioning worker encrypts credential payloads using AES-256-GCM.
	// The key must be exactly 32 bytes; pad or truncate here.
	masterKeyRaw := []byte(getEnv("PAYLOAD_MASTER_KEY", ""))
	if len(masterKeyRaw) < 32 {
		// Pad with zeros — production MUST supply a proper 32-byte key.
		padded := make([]byte, 32)
		copy(padded, masterKeyRaw)
		masterKeyRaw = padded
		if getEnv("ENVIRONMENT", "development") != "development" {
			log.Printf("WARNING: PAYLOAD_MASTER_KEY is shorter than 32 bytes in non-dev environment")
		}
	} else {
		masterKeyRaw = masterKeyRaw[:32]
	}

	// ---- Start all workers --------------------------------------------------
	log.Printf("workers: starting all workers")

	go func() {
		log.Printf("workers: [provisioning] starting")
		provisioning.NewWorker(pool, masterKeyRaw).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [health] starting")
		health.NewWorker(pool, rdb).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [backup] starting")
		backup.NewWorker(pool).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [malware] starting")
		malware.NewWorker(pool, rdb).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [billing] starting")
		billing.NewWorker(pool).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [cleanup] starting")
		cleanup.NewWorker(pool).Run(ctx)
	}()

	go func() {
		log.Printf("workers: [metrics] starting")
		metrics.NewWorker(pool).Run(ctx)
	}()

	// Block until SIGTERM or SIGINT is received.
	<-ctx.Done()
	log.Printf("workers: received shutdown signal — all workers stopping")
}

// mustEnv returns the value of an environment variable or fatally exits.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("workers: required env var %s is not set", key)
	}
	return v
}

// getEnv returns the env var value or a default if not set.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}


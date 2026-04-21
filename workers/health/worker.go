// Package health provides the site health monitoring worker for DNSFox v2.
// worker.go probes all active sites every 60 seconds, tracks consecutive
// failures per site in Redis, and transitions site status in the DB when
// thresholds are crossed.
package health

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// probeInterval is how often all active sites are probed.
	probeInterval = 60 * time.Second

	// probeParallelism caps the number of concurrent HTTP probes.
	probeParallelism = 20

	// failuresBeforeError is the number of consecutive failures that triggers
	// a site status transition to 'error'.
	failuresBeforeError = 5

	// failuresBeforeRestart is the consecutive failure count at which a warden
	// nginx-reload job is enqueued to attempt recovery.
	failuresBeforeRestart = 3

	// hourlyAggregationInterval how often we roll up hourly summaries.
	hourlyAggregationInterval = 1 * time.Hour

	// dailyAggregationInterval how often we roll up daily summaries.
	dailyAggregationInterval = 24 * time.Hour
)

// siteRow is a minimal projection of the instances table.
type siteRow struct {
	ID       string
	ServerID string
	Domain   string
	Status   string
}

// Worker orchestrates site probing, failure tracking, and status transitions.
type Worker struct {
	pool    *pgxpool.Pool
	rdb     *redis.Client
	checker *Checker
	agg     *Aggregator
}

// NewWorker creates a Worker.
func NewWorker(pool *pgxpool.Pool, rdb *redis.Client) *Worker {
	return &Worker{
		pool:    pool,
		rdb:     rdb,
		checker: NewChecker(pool),
		agg:     NewAggregator(pool),
	}
}

// Run starts all tickers and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[health] worker started (probe=%s, parallelism=%d)", probeInterval, probeParallelism)

	probeTicker := time.NewTicker(probeInterval)
	hourlyTicker := time.NewTicker(hourlyAggregationInterval)
	dailyTicker := time.NewTicker(dailyAggregationInterval)
	defer probeTicker.Stop()
	defer hourlyTicker.Stop()
	defer dailyTicker.Stop()

	for {
		select {
		case <-probeTicker.C:
			w.runProbes(ctx)
		case <-hourlyTicker.C:
			w.agg.RollupHourly(ctx)
			w.agg.PruneRaw(ctx)
		case <-dailyTicker.C:
			w.agg.RollupDaily(ctx)
			w.agg.PruneHourly(ctx)
		case <-ctx.Done():
			log.Printf("[health] worker shutting down")
			return
		}
	}
}

// runProbes loads all active sites and issues parallel HTTP probes.
func (w *Worker) runProbes(ctx context.Context) {
	sites, err := w.loadActiveSites(ctx)
	if err != nil {
		log.Printf("[health] load sites error: %v", err)
		return
	}
	if len(sites) == 0 {
		return
	}

	sem := make(chan struct{}, probeParallelism)
	var wg sync.WaitGroup

	for _, s := range sites {
		s := s // loop-variable capture
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.probeAndReact(ctx, s)
		}()
	}
	wg.Wait()
}

// probeAndReact probes one site and updates DB + Redis based on the result.
func (w *Worker) probeAndReact(ctx context.Context, s siteRow) {
	res := w.checker.Probe(ctx, s.ID, s.Domain)

	failKey := fmt.Sprintf("health:failures:%s", s.ID)

	if res.Up {
		// Site recovered — clear Redis failure counter and restore 'active' if
		// the site was in error state.
		if err := w.rdb.Del(ctx, failKey).Err(); err != nil {
			log.Printf("[health] redis del error site=%s: %v", s.ID, err)
		}
		if s.Status == "error" {
			if _, err := w.pool.Exec(ctx, `
				UPDATE instances SET status='active', error_message=NULL, updated_at=NOW()
				WHERE id=$1
			`, s.ID); err != nil {
				log.Printf("[health] restore active error site=%s: %v", s.ID, err)
			}
			log.Printf("[health] site=%s recovered — restored to active", s.ID)
		}
		return
	}

	// Probe failed — increment failure counter in Redis.
	count, err := w.rdb.Incr(ctx, failKey).Result()
	if err != nil {
		log.Printf("[health] redis incr error site=%s: %v", s.ID, err)
		return
	}
	// Set a TTL so the key self-expires if the worker is down for a long time.
	w.rdb.Expire(ctx, failKey, 2*time.Hour) //nolint:errcheck

	log.Printf("[health] site=%s probe failed consecutive=%d err=%v", s.ID, count, res.Err)

	// 3+ failures → enqueue a warden nginx-reload restart job.
	if count >= failuresBeforeRestart {
		w.enqueueRestart(ctx, s)
	}

	// 5+ failures → mark site as error.
	if count >= failuresBeforeError && s.Status != "error" {
		if _, err := w.pool.Exec(ctx, `
			UPDATE instances SET status='error',
			       error_message=$2, updated_at=NOW()
			WHERE id=$1
		`, s.ID, fmt.Sprintf("health probe: %d consecutive failures", count)); err != nil {
			log.Printf("[health] mark error site=%s: %v", s.ID, err)
		}
	}
}

// enqueueRestart inserts an agent_jobs row to reload nginx on the site's server.
// Idempotent within 5 minutes via Redis dedup key.
func (w *Worker) enqueueRestart(ctx context.Context, s siteRow) {
	dedupKey := fmt.Sprintf("health:restart_dedup:%s", s.ID)
	// Only enqueue one restart per 5-minute window to avoid storm.
	set, err := w.rdb.SetNX(ctx, dedupKey, "1", 5*time.Minute).Result()
	if err != nil || !set {
		return
	}

	// $2 is a uuid column binding (instance_id) and can't also be inferred
	// inside jsonb_build_object (VARIADIC "any"). Pass site_id separately
	// as $3::text for the JSON payload; $4 becomes the domain.
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO agent_jobs
			(id, server_id, instance_id, job_type, priority, payload, status, max_attempts)
		VALUES
			(gen_random_uuid(), $1, $2, 'reload_nginx', 5,
			 jsonb_build_object('site_id', $3::text, 'domain', $4::text), 'pending', 3)
	`, s.ServerID, s.ID, s.ID, s.Domain); err != nil {
		log.Printf("[health] enqueue restart error site=%s: %v", s.ID, err)
	}
}

// loadActiveSites returns all instances that are not deleted or suspended.
func (w *Worker) loadActiveSites(ctx context.Context) ([]siteRow, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, COALESCE(server_id::text,''), domain, status
		FROM instances
		WHERE status NOT IN ('deleted', 'suspended')
	`)
	if err != nil {
		return nil, fmt.Errorf("health: load sites: %w", err)
	}
	defer rows.Close()

	var sites []siteRow
	for rows.Next() {
		var s siteRow
		if err := rows.Scan(&s.ID, &s.ServerID, &s.Domain, &s.Status); err != nil {
			return nil, fmt.Errorf("health: scan site: %w", err)
		}
		sites = append(sites, s)
	}
	return sites, rows.Err()
}

// consecutiveFailures returns the current Redis failure count for a site.
// Used by tests and admin endpoints.
func (w *Worker) consecutiveFailures(ctx context.Context, siteID string) int {
	val, err := w.rdb.Get(ctx, fmt.Sprintf("health:failures:%s", siteID)).Result()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(val)
	return n
}

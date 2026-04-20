// Package health provides the site health monitoring worker for DNSFox v2.
// aggregator.go rolls up raw uptime_checks into hourly and daily summaries
// and prunes old raw data to control table growth.
package health

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Aggregator rolls up uptime_checks data and prunes stale rows.
type Aggregator struct {
	pool *pgxpool.Pool
}

// NewAggregator returns a new Aggregator.
func NewAggregator(pool *pgxpool.Pool) *Aggregator {
	return &Aggregator{pool: pool}
}

// RollupHourly aggregates raw uptime_checks from the last two hours into
// hourly_uptime_summary.  The upsert lets it be re-run safely.
func (a *Aggregator) RollupHourly(ctx context.Context) {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO hourly_uptime_summary
			(instance_id, hour, total_checks, up_checks, avg_response_ms)
		SELECT
			instance_id,
			date_trunc('hour', checked_at) AS hour,
			COUNT(*)                        AS total_checks,
			COUNT(*) FILTER (WHERE status = 'up') AS up_checks,
			AVG(response_ms)               AS avg_response_ms
		FROM uptime_checks
		WHERE checked_at >= NOW() - INTERVAL '2 hours'
		  AND checked_at <  date_trunc('hour', NOW())
		GROUP BY instance_id, date_trunc('hour', checked_at)
		ON CONFLICT (instance_id, hour)
		DO UPDATE SET
			total_checks   = EXCLUDED.total_checks,
			up_checks      = EXCLUDED.up_checks,
			avg_response_ms = EXCLUDED.avg_response_ms,
			updated_at     = NOW()
	`)
	if err != nil {
		log.Printf("[health/aggregator] hourly rollup error: %v", err)
		return
	}
	log.Printf("[health/aggregator] hourly rollup complete")
}

// RollupDaily aggregates hourly_uptime_summary into daily_uptime_summary.
// Runs once per day (caller decides timing).
func (a *Aggregator) RollupDaily(ctx context.Context) {
	_, err := a.pool.Exec(ctx, `
		INSERT INTO daily_uptime_summary
			(instance_id, day, total_checks, up_checks, avg_response_ms)
		SELECT
			instance_id,
			date_trunc('day', hour) AS day,
			SUM(total_checks)       AS total_checks,
			SUM(up_checks)          AS up_checks,
			AVG(avg_response_ms)    AS avg_response_ms
		FROM hourly_uptime_summary
		WHERE hour >= date_trunc('day', NOW()) - INTERVAL '1 day'
		  AND hour <  date_trunc('day', NOW())
		GROUP BY instance_id, date_trunc('day', hour)
		ON CONFLICT (instance_id, day)
		DO UPDATE SET
			total_checks    = EXCLUDED.total_checks,
			up_checks       = EXCLUDED.up_checks,
			avg_response_ms = EXCLUDED.avg_response_ms,
			updated_at      = NOW()
	`)
	if err != nil {
		log.Printf("[health/aggregator] daily rollup error: %v", err)
		return
	}
	log.Printf("[health/aggregator] daily rollup complete")
}

// PruneRaw deletes raw uptime_checks older than 48 hours.
func (a *Aggregator) PruneRaw(ctx context.Context) {
	tag, err := a.pool.Exec(ctx, `
		DELETE FROM uptime_checks WHERE checked_at < NOW() - INTERVAL '48 hours'
	`)
	if err != nil {
		log.Printf("[health/aggregator] prune raw error: %v", err)
		return
	}
	log.Printf("[health/aggregator] pruned %d raw uptime_checks rows", tag.RowsAffected())
}

// PruneHourly deletes hourly summaries older than 30 days.
func (a *Aggregator) PruneHourly(ctx context.Context) {
	tag, err := a.pool.Exec(ctx, `
		DELETE FROM hourly_uptime_summary WHERE hour < NOW() - INTERVAL '30 days'
	`)
	if err != nil {
		log.Printf("[health/aggregator] prune hourly error: %v", err)
		return
	}
	log.Printf("[health/aggregator] pruned %d hourly_uptime_summary rows", tag.RowsAffected())
}

// Package metrics provides the metrics aggregation worker for DNSFox v2.
// worker.go rolls up raw resource_metrics into hourly and daily summaries and
// prunes stale raw data according to the platform retention policy:
//   - raw resource_metrics: 48 hours
//   - hourly_metrics_summary: 30 days
//   - daily_metrics_summary: kept indefinitely (1 year effectively via daily aggregation)
package metrics

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// hourlyInterval is how often hourly aggregation runs.
	hourlyInterval = 1 * time.Hour

	// dailyInterval is how often daily aggregation runs (midnight UTC check).
	dailyInterval = 1 * time.Minute // tick every minute, check for midnight

	// rawRetention is how long raw resource_metrics rows are kept.
	rawRetention = 48 * time.Hour

	// hourlyRetention is how long hourly summaries are kept.
	hourlyRetention = 30 * 24 * time.Hour
)

// Worker aggregates resource_metrics into summary tables and prunes old data.
type Worker struct {
	pool *pgxpool.Pool
}

// NewWorker creates a metrics Worker.
func NewWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{pool: pool}
}

// Run starts the aggregation tickers and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[metrics] worker started (hourly aggregation + daily at midnight UTC)")

	hourlyTicker := time.NewTicker(hourlyInterval)
	// Use a 1-minute ticker to detect midnight without drift.
	midnightTicker := time.NewTicker(dailyInterval)
	defer hourlyTicker.Stop()
	defer midnightTicker.Stop()

	var lastDailyDate string

	for {
		select {
		case <-hourlyTicker.C:
			w.aggregateHourly(ctx)
			w.pruneRaw(ctx)

		case <-midnightTicker.C:
			// Run daily aggregation once at midnight UTC.
			now := time.Now().UTC()
			today := now.Format("2006-01-02")
			if now.Hour() == 0 && now.Minute() < 5 && lastDailyDate != today {
				lastDailyDate = today
				w.aggregateDaily(ctx)
				w.pruneHourly(ctx)
			}

		case <-ctx.Done():
			log.Printf("[metrics] worker shutting down")
			return
		}
	}
}

// aggregateHourly inserts or updates hourly_metrics_summary for the previous
// full hour using raw resource_metrics data.
// ON CONFLICT allows safe re-runs if the worker restarts mid-hour.
func (w *Worker) aggregateHourly(ctx context.Context) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO hourly_metrics_summary
			(instance_id, hour, avg_cpu, max_cpu, avg_ram, avg_disk, request_count)
		SELECT
			instance_id,
			date_trunc('hour', created_at)    AS hour,
			AVG(cpu_percentage)               AS avg_cpu,
			MAX(cpu_percentage)               AS max_cpu,
			AVG(ram_percentage)               AS avg_ram,
			AVG(disk_usage_gb)                AS avg_disk,
			COUNT(*)                          AS request_count
		FROM resource_metrics
		WHERE created_at >= NOW() - INTERVAL '2 hours'
		  AND created_at <  date_trunc('hour', NOW())
		GROUP BY instance_id, date_trunc('hour', created_at)
		ON CONFLICT (instance_id, hour)
		DO UPDATE SET
			avg_cpu       = EXCLUDED.avg_cpu,
			max_cpu       = EXCLUDED.max_cpu,
			avg_ram       = EXCLUDED.avg_ram,
			avg_disk      = EXCLUDED.avg_disk,
			request_count = EXCLUDED.request_count,
			updated_at    = NOW()
	`)
	if err != nil {
		log.Printf("[metrics] hourly aggregation error: %v", err)
		return
	}
	log.Printf("[metrics] hourly aggregation complete")
}

// aggregateDaily inserts or updates daily_metrics_summary from hourly summaries.
// Runs once at midnight UTC for the previous day.
func (w *Worker) aggregateDaily(ctx context.Context) {
	_, err := w.pool.Exec(ctx, `
		INSERT INTO daily_metrics_summary
			(instance_id, day, avg_cpu, max_cpu, avg_ram, avg_disk, request_count)
		SELECT
			instance_id,
			date_trunc('day', hour)       AS day,
			AVG(avg_cpu)                  AS avg_cpu,
			MAX(max_cpu)                  AS max_cpu,
			AVG(avg_ram)                  AS avg_ram,
			AVG(avg_disk)                 AS avg_disk,
			SUM(request_count)            AS request_count
		FROM hourly_metrics_summary
		WHERE hour >= date_trunc('day', NOW()) - INTERVAL '1 day'
		  AND hour <  date_trunc('day', NOW())
		GROUP BY instance_id, date_trunc('day', hour)
		ON CONFLICT (instance_id, day)
		DO UPDATE SET
			avg_cpu       = EXCLUDED.avg_cpu,
			max_cpu       = EXCLUDED.max_cpu,
			avg_ram       = EXCLUDED.avg_ram,
			avg_disk      = EXCLUDED.avg_disk,
			request_count = EXCLUDED.request_count,
			updated_at    = NOW()
	`)
	if err != nil {
		log.Printf("[metrics] daily aggregation error: %v", err)
		return
	}
	log.Printf("[metrics] daily aggregation complete")
}

// pruneRaw deletes raw resource_metrics rows older than 48 hours.
// TimescaleDB drop_chunks handles this automatically if the hypertable is set up,
// but we include a fallback DELETE for non-TimescaleDB deployments.
func (w *Worker) pruneRaw(ctx context.Context) {
	tag, err := w.pool.Exec(ctx, `
		DELETE FROM resource_metrics
		WHERE created_at < NOW() - INTERVAL '48 hours'
	`)
	if err != nil {
		log.Printf("[metrics] prune raw error: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		log.Printf("[metrics] pruned %d raw resource_metrics rows", tag.RowsAffected())
	}
}

// pruneHourly deletes hourly_metrics_summary rows older than 30 days.
func (w *Worker) pruneHourly(ctx context.Context) {
	tag, err := w.pool.Exec(ctx, `
		DELETE FROM hourly_metrics_summary
		WHERE hour < NOW() - INTERVAL '30 days'
	`)
	if err != nil {
		log.Printf("[metrics] prune hourly error: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		log.Printf("[metrics] pruned %d hourly_metrics_summary rows", tag.RowsAffected())
	}
}

// Package cleanup provides the daily maintenance worker for DNSFox v2.
// worker.go runs at 2 AM UTC and performs housekeeping tasks:
//   - Deletes expired support PINs
//   - Prunes old completed/failed provisioning jobs
//   - Logs disk usage warnings for sites exceeding 80% of plan quota
//   - Triggers orphaned site directory scans via Warden GetServerStats
package cleanup

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// cleanupHour is the UTC hour at which daily cleanup runs.
	cleanupHour = 2

	// jobRetentionDays is how long completed/failed provisioning jobs are kept.
	jobRetentionDays = 30

	// diskWarningThreshold is the fraction of plan storage at which we log a warning.
	diskWarningThreshold = 0.8
)

// Worker runs daily maintenance tasks.
type Worker struct {
	pool *pgxpool.Pool
}

// NewWorker creates a cleanup Worker.
func NewWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{pool: pool}
}

// Run starts the daily cleanup ticker and blocks until ctx is cancelled.
// It uses a 1-minute ticker to check the wall clock so we fire within
// 1 minute of 2 AM UTC rather than drifting over time.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[cleanup] worker started (runs daily at %02d:00 UTC)", cleanupHour)

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	var lastRunDate string // tracks "YYYY-MM-DD" of last run so we run once/day

	for {
		select {
		case <-ticker.C:
			now := time.Now().UTC()
			today := now.Format("2006-01-02")
			// Fire once per day when the clock hits the target hour.
			if now.Hour() == cleanupHour && lastRunDate != today {
				lastRunDate = today
				log.Printf("[cleanup] starting daily cleanup run for %s", today)
				w.run(ctx)
			}
		case <-ctx.Done():
			log.Printf("[cleanup] worker shutting down")
			return
		}
	}
}

// run executes all daily cleanup tasks sequentially.
func (w *Worker) run(ctx context.Context) {
	w.deleteSupportPins(ctx)
	w.pruneOldJobs(ctx)
	w.logDiskWarnings(ctx)
}

// deleteSupportPins removes support PINs that have passed their expiry time.
func (w *Worker) deleteSupportPins(ctx context.Context) {
	tag, err := w.pool.Exec(ctx,
		`DELETE FROM support_pins WHERE expires_at < NOW()`)
	if err != nil {
		log.Printf("[cleanup] delete support pins error: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		log.Printf("[cleanup] deleted %d expired support PINs", tag.RowsAffected())
	}
}

// pruneOldJobs deletes completed and failed provisioning jobs older than 30 days.
// Keeping recent failures for 30 days gives ops enough time to investigate.
func (w *Worker) pruneOldJobs(ctx context.Context) {
	tag, err := w.pool.Exec(ctx, `
		DELETE FROM provisioning_jobs
		WHERE created_at < NOW() - ($1 || ' days')::INTERVAL
		  AND status IN ('completed', 'failed')
	`, jobRetentionDays)
	if err != nil {
		log.Printf("[cleanup] prune old jobs error: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		log.Printf("[cleanup] pruned %d old provisioning jobs", tag.RowsAffected())
	}
}

// logDiskWarnings fetches sites whose disk_usage_gb exceeds 80% of their plan
// allocation and logs a warning.  This surfaces potential disk-full incidents
// before they cause downtime.  Actual enforcement is out of scope for this worker.
func (w *Worker) logDiskWarnings(ctx context.Context) {
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, domain,
		       COALESCE(disk_usage_gb,0),
		       storage_gb
		FROM instances
		WHERE status = 'active'
		  AND storage_gb > 0
		  AND COALESCE(disk_usage_gb,0) > storage_gb * $1
	`, diskWarningThreshold)
	if err != nil {
		log.Printf("[cleanup] disk warning query error: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var siteID, domain string
		var diskUsage, storageGB float64
		if err := rows.Scan(&siteID, &domain, &diskUsage, &storageGB); err != nil {
			continue
		}
		pct := (diskUsage / storageGB) * 100
		log.Printf("[cleanup] DISK WARNING site=%s domain=%s usage=%.2f/%.0fGB (%.0f%%)",
			siteID, domain, diskUsage, storageGB, pct)
		count++
	}
	if count > 0 {
		log.Printf("[cleanup] %d sites approaching disk limit", count)
	}
}

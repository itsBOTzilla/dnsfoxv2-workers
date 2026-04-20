// Package backup provides the backup scheduling and pruning worker for DNSFox v2.
// worker.go runs a 5-minute schedule check to dispatch backup jobs to Warden
// agents via the agent_jobs table, and a 24-hour prune cycle.
package backup

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// scheduleCheckInterval is how often we check for sites due for backup.
	scheduleCheckInterval = 5 * time.Minute

	// pruneInterval is how often the retention pruner runs.
	pruneInterval = 24 * time.Hour
)

// Worker orchestrates scheduled and on-demand backups plus retention pruning.
type Worker struct {
	pool      *pgxpool.Pool
	scheduler *Scheduler
	pruner    *Pruner
}

// NewWorker creates a backup Worker.
func NewWorker(pool *pgxpool.Pool) *Worker {
	return &Worker{
		pool:      pool,
		scheduler: NewScheduler(pool),
		pruner:    NewPruner(pool),
	}
}

// Run starts the backup schedule ticker and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[backup] worker started (schedule=%s, prune=%s)",
		scheduleCheckInterval, pruneInterval)

	scheduleTicker := time.NewTicker(scheduleCheckInterval)
	pruneTicker := time.NewTicker(pruneInterval)
	defer scheduleTicker.Stop()
	defer pruneTicker.Stop()

	// Run the schedule check immediately on startup so first backups fire
	// without waiting 5 minutes.
	w.runSchedule(ctx)

	for {
		select {
		case <-scheduleTicker.C:
			w.runSchedule(ctx)
		case <-pruneTicker.C:
			w.pruner.PruneExpired(ctx)
		case <-ctx.Done():
			log.Printf("[backup] worker shutting down")
			return
		}
	}
}

// runSchedule checks for both scheduled and on-demand backups in one pass.
func (w *Worker) runSchedule(ctx context.Context) {
	// Scheduled backups — sites whose last backup exceeds their plan interval.
	due, err := w.scheduler.GetSitesDueForBackup(ctx)
	if err != nil {
		log.Printf("[backup] get due sites error: %v", err)
	} else {
		for _, s := range due {
			if err := w.enqueueBackup(ctx, s, false); err != nil {
				log.Printf("[backup] enqueue backup error site=%s: %v", s.SiteID, err)
			}
		}
		if len(due) > 0 {
			log.Printf("[backup] enqueued %d scheduled backup jobs", len(due))
		}
	}

	// On-demand backups — process immediately regardless of schedule.
	onDemand, err := w.scheduler.GetOnDemandSites(ctx)
	if err != nil {
		log.Printf("[backup] get on-demand sites error: %v", err)
		return
	}
	for _, s := range onDemand {
		if err := w.enqueueBackup(ctx, s, true); err != nil {
			log.Printf("[backup] enqueue on-demand error site=%s: %v", s.SiteID, err)
		}
	}
	if len(onDemand) > 0 {
		log.Printf("[backup] enqueued %d on-demand backup jobs", len(onDemand))
	}
}

// enqueueBackup inserts a backup record (status=pending) and an agent_jobs row
// so Warden picks up the work.  For on-demand backups it also marks the
// triggering backups record as 'queued' so we don't re-trigger it.
func (w *Worker) enqueueBackup(ctx context.Context, s SiteBackupInfo, onDemand bool) error {
	// Create a pending backup record so the dashboard shows it immediately.
	var backupID string
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO backups
			(id, instance_id, customer_id, type, storage_location, status)
		VALUES (gen_random_uuid(), $1, $2, 'full', 'b2', 'pending')
		RETURNING id::text
	`, s.SiteID, s.CustomerID).Scan(&backupID); err != nil {
		return err
	}

	// Insert the agent job.  Warden picks it up via heartbeat job polling.
	_, err := w.pool.Exec(ctx, `
		INSERT INTO agent_jobs
			(id, server_id, instance_id, job_type, priority,
			 payload, status, max_attempts)
		VALUES
			(gen_random_uuid(), $1, $2, 'backup_site', 3,
			 jsonb_build_object(
				 'backup_id', $3,
				 'site_id', $2,
				 'domain', $4,
				 'plan', $5,
				 'on_demand', $6
			 ), 'pending', 2)
	`, s.ServerID, s.SiteID, backupID, s.Domain, s.Plan, onDemand)
	if err != nil {
		return err
	}

	// For on-demand requests, flip the originating backup record from
	// 'requested' to 'queued' so the scheduler won't re-trigger it.
	if onDemand {
		if _, err := w.pool.Exec(ctx, `
			UPDATE backups
			SET status = 'queued'
			WHERE instance_id = $1
			  AND on_demand = TRUE
			  AND status = 'requested'
			  AND deleted_at IS NULL
		`, s.SiteID); err != nil {
			log.Printf("[backup] mark on-demand queued error site=%s: %v", s.SiteID, err)
		}
	}

	log.Printf("[backup] queued backup=%s site=%s on_demand=%v", backupID, s.SiteID, onDemand)
	return nil
}

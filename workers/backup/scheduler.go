// Package backup provides the backup scheduling and pruning worker for DNSFox v2.
// scheduler.go queries which sites are due for a scheduled backup based on
// their plan's backup interval.
package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// planIntervals maps plan names to the minimum elapsed time between backups.
// All current plans back up daily.  Add tiers here when plans diverge.
var planIntervals = map[string]time.Duration{
	"fox":   24 * time.Hour,
	"swift": 24 * time.Hour,
	"apex":  24 * time.Hour,
	"titan": 24 * time.Hour,
}

// defaultInterval is used when the site's plan is not in planIntervals.
const defaultInterval = 24 * time.Hour

// SiteBackupInfo carries the fields needed to schedule a backup job.
type SiteBackupInfo struct {
	SiteID     string
	CustomerID string
	ServerID   string
	Domain     string
	Plan       string
}

// Scheduler queries for sites that are due for a backup.
type Scheduler struct {
	pool *pgxpool.Pool
}

// NewScheduler returns a Scheduler backed by the given pool.
func NewScheduler(pool *pgxpool.Pool) *Scheduler {
	return &Scheduler{pool: pool}
}

// GetSitesDueForBackup returns all active sites whose last backup completed more
// than plan_interval ago (or have never been backed up).
func (s *Scheduler) GetSitesDueForBackup(ctx context.Context) ([]SiteBackupInfo, error) {
	// We compute plan_interval inside the query by joining on plan name.
	// Fallback to 24h for unknown plans.  The CASE expression converts the
	// plan string to a PG interval so the arithmetic stays in the DB.
	rows, err := s.pool.Query(ctx, `
		SELECT
			i.id::text,
			COALESCE(i.customer_id::text,''),
			COALESCE(i.server_id::text,''),
			i.domain,
			COALESCE(i.plan,'fox')
		FROM instances i
		WHERE i.status = 'active'
		  AND NOT EXISTS (
			  -- Already has a recent completed backup within the plan interval.
			  SELECT 1 FROM backups b
			  WHERE b.instance_id = i.id
			    AND b.status = 'completed'
			    AND b.deleted_at IS NULL
			    AND b.created_at > NOW() - CASE i.plan
					WHEN 'titan' THEN INTERVAL '24 hours'
					WHEN 'apex'  THEN INTERVAL '24 hours'
					WHEN 'swift' THEN INTERVAL '24 hours'
					ELSE              INTERVAL '24 hours'
				END
		  )
		  AND NOT EXISTS (
			  -- Already has a backup in flight — don't pile up duplicates.
			  SELECT 1 FROM backups b
			  WHERE b.instance_id = i.id
			    AND b.status IN ('pending', 'queued', 'in_progress')
			    AND b.deleted_at IS NULL
		  )
	`)
	if err != nil {
		return nil, fmt.Errorf("scheduler: query sites due: %w", err)
	}
	defer rows.Close()

	var out []SiteBackupInfo
	for rows.Next() {
		var si SiteBackupInfo
		if err := rows.Scan(&si.SiteID, &si.CustomerID, &si.ServerID,
			&si.Domain, &si.Plan); err != nil {
			return nil, fmt.Errorf("scheduler: scan: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// GetOnDemandSites returns sites that have a pending on-demand backup request.
// The backups table uses on_demand=true + status='requested' to signal these.
func (s *Scheduler) GetOnDemandSites(ctx context.Context) ([]SiteBackupInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT
			i.id::text,
			COALESCE(i.customer_id::text,''),
			COALESCE(i.server_id::text,''),
			i.domain,
			COALESCE(i.plan,'fox')
		FROM backups b
		JOIN instances i ON i.id = b.instance_id
		WHERE b.on_demand = TRUE
		  AND b.status = 'requested'
		  AND b.deleted_at IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("scheduler: on-demand query: %w", err)
	}
	defer rows.Close()

	var out []SiteBackupInfo
	for rows.Next() {
		var si SiteBackupInfo
		if err := rows.Scan(&si.SiteID, &si.CustomerID, &si.ServerID,
			&si.Domain, &si.Plan); err != nil {
			return nil, fmt.Errorf("scheduler: on-demand scan: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// IntervalFor returns the backup interval for a plan, defaulting to 24h.
func IntervalFor(plan string) time.Duration {
	if d, ok := planIntervals[plan]; ok {
		return d
	}
	return defaultInterval
}

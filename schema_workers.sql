-- DNSFox v2 workers: supplemental schema migration
-- Run this after schema_sites.sql, schema_billing.sql, and schema_monitoring.sql.
-- All statements are idempotent (CREATE TABLE IF NOT EXISTS, ADD COLUMN IF NOT EXISTS).

-- ---------------------------------------------------------------------------
-- provisioning_jobs: worker_id column for distributed claiming
-- ---------------------------------------------------------------------------
ALTER TABLE provisioning_jobs
    ADD COLUMN IF NOT EXISTS worker_id TEXT;

-- ---------------------------------------------------------------------------
-- instances: disk_usage_gb for capacity tracking
-- ---------------------------------------------------------------------------
ALTER TABLE instances
    ADD COLUMN IF NOT EXISTS disk_usage_gb DECIMAL(10,2) DEFAULT 0;

-- ---------------------------------------------------------------------------
-- backups: B2 object references and on-demand flag
-- ---------------------------------------------------------------------------
ALTER TABLE backups
    ADD COLUMN IF NOT EXISTS b2_file_id   TEXT,
    ADD COLUMN IF NOT EXISTS b2_file_name TEXT,
    ADD COLUMN IF NOT EXISTS on_demand    BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS backups_on_demand_idx
    ON backups (instance_id, on_demand, status)
    WHERE on_demand = TRUE;

-- ---------------------------------------------------------------------------
-- subscriptions: dunning state machine columns
-- ---------------------------------------------------------------------------
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS dunning_stage          TEXT NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS dunning_started_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dunning_next_action_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS suspended_at           TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS subscriptions_dunning_idx
    ON subscriptions (dunning_stage, dunning_next_action_at)
    WHERE dunning_stage NOT IN ('none', 'canceled');

-- ---------------------------------------------------------------------------
-- stripe_events: inbound Stripe webhook events queue
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS stripe_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type      TEXT NOT NULL,
    stripe_event_id TEXT UNIQUE,
    payload         JSONB,
    processed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS stripe_events_unprocessed_idx
    ON stripe_events (created_at ASC)
    WHERE processed_at IS NULL;

CREATE INDEX IF NOT EXISTS stripe_events_stripe_id_idx
    ON stripe_events (stripe_event_id);

-- ---------------------------------------------------------------------------
-- hourly_uptime_summary: rolled-up uptime data (kept 30 days)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS hourly_uptime_summary (
    instance_id     UUID NOT NULL,
    hour            TIMESTAMPTZ NOT NULL,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    up_checks       INTEGER NOT NULL DEFAULT 0,
    avg_response_ms INTEGER,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instance_id, hour)
);

CREATE INDEX IF NOT EXISTS hourly_uptime_hour_idx ON hourly_uptime_summary (hour DESC);

-- ---------------------------------------------------------------------------
-- daily_uptime_summary: further rolled-up daily data (kept 1 year)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS daily_uptime_summary (
    instance_id     UUID NOT NULL,
    day             TIMESTAMPTZ NOT NULL,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    up_checks       INTEGER NOT NULL DEFAULT 0,
    avg_response_ms INTEGER,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instance_id, day)
);

CREATE INDEX IF NOT EXISTS daily_uptime_day_idx ON daily_uptime_summary (day DESC);

-- ---------------------------------------------------------------------------
-- hourly_metrics_summary: hourly resource metric rollups (kept 30 days)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS hourly_metrics_summary (
    instance_id   UUID NOT NULL,
    hour          TIMESTAMPTZ NOT NULL,
    avg_cpu       DECIMAL(5,2),
    max_cpu       DECIMAL(5,2),
    avg_ram       DECIMAL(5,2),
    avg_disk      DECIMAL(10,2),
    request_count BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instance_id, hour)
);

CREATE INDEX IF NOT EXISTS hourly_metrics_hour_idx ON hourly_metrics_summary (hour DESC);

-- ---------------------------------------------------------------------------
-- daily_metrics_summary: daily resource metric rollups (kept 1 year)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS daily_metrics_summary (
    instance_id   UUID NOT NULL,
    day           TIMESTAMPTZ NOT NULL,
    avg_cpu       DECIMAL(5,2),
    max_cpu       DECIMAL(5,2),
    avg_ram       DECIMAL(5,2),
    avg_disk      DECIMAL(10,2),
    request_count BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instance_id, day)
);

CREATE INDEX IF NOT EXISTS daily_metrics_day_idx ON daily_metrics_summary (day DESC);

-- ---------------------------------------------------------------------------
-- monitoring_incidents: malware and downtime incident records
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS monitoring_incidents (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id UUID,
    type        TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'info',
    title       TEXT NOT NULL,
    description TEXT,
    status      TEXT NOT NULL DEFAULT 'open',
    resolved_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS monitoring_incidents_instance_idx
    ON monitoring_incidents (instance_id, created_at DESC);

CREATE INDEX IF NOT EXISTS monitoring_incidents_status_idx
    ON monitoring_incidents (status)
    WHERE status = 'open';

-- ---------------------------------------------------------------------------
-- audit_logs: platform-wide event trail (referenced by dunning + billing)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_logs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action      TEXT NOT NULL,
    target_type TEXT,
    target_id   TEXT,
    user_id     UUID,
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS audit_logs_target_idx ON audit_logs (target_type, target_id);
CREATE INDEX IF NOT EXISTS audit_logs_action_idx ON audit_logs (action, created_at DESC);

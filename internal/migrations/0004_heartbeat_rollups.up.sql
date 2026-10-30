-- 0004_heartbeat_rollups.up.sql — pre-aggregated heartbeat rollups.
-- Long-range uptime/latency queries read these compact per-hour / per-day rows
-- instead of scanning the raw heartbeats table. A background job (see
-- internal/report) fills them; they are safe to rebuild at any time.

BEGIN;

CREATE TABLE heartbeat_rollups_hourly (
    monitor_id     bigint      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    bucket_at      timestamptz NOT NULL, -- truncated to the hour (UTC)
    up_count       integer     NOT NULL DEFAULT 0,
    down_count     integer     NOT NULL DEFAULT 0,
    avg_latency_ms integer     NOT NULL DEFAULT 0,
    min_latency_ms integer     NOT NULL DEFAULT 0,
    max_latency_ms integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (monitor_id, bucket_at)
);

CREATE INDEX heartbeat_rollups_hourly_bucket_idx ON heartbeat_rollups_hourly (bucket_at);

CREATE TABLE heartbeat_rollups_daily (
    monitor_id     bigint      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    bucket_at      timestamptz NOT NULL, -- truncated to the day (UTC)
    up_count       integer     NOT NULL DEFAULT 0,
    down_count     integer     NOT NULL DEFAULT 0,
    avg_latency_ms integer     NOT NULL DEFAULT 0,
    min_latency_ms integer     NOT NULL DEFAULT 0,
    max_latency_ms integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (monitor_id, bucket_at)
);

CREATE INDEX heartbeat_rollups_daily_bucket_idx ON heartbeat_rollups_daily (bucket_at);

COMMIT;

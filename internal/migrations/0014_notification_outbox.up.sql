-- 0014_notification_outbox.up.sql — durable hand-off for alert events.
--
-- The dispatcher consumed events through an in-memory queue only. Three ways an
-- alert was lost, all silent:
--
--   * a full queue. The queue is bounded on purpose (an unbounded one just
--     converts a delivery backlog into an OOM), so past the bound the event was
--     dropped after a short wait. That is the moment a fleet-wide incident is
--     unfolding, i.e. exactly when the alerts matter most.
--   * a crash or a SIGKILL between accepting an event and delivering it. The
--     queue lives in RAM; nothing survived.
--   * every provider for an event failing. The audit log recorded the failure
--     and the event was then dropped; nothing ever tried again.
--
-- For a monitoring system the alert IS the product, so an event is written here
-- the moment it is accepted and removed only once it is genuinely finished —
-- delivered, or deliberately not delivered (muted, quiet hours, no channels).
-- Anything still present is by definition unfinished work: the retry worker
-- leases rows whose claim has lapsed and runs them again, which covers the
-- restart case with no extra machinery.
--
-- The row holds the whole event rather than a rendered message: channel
-- assignments, mutes and maintenance windows are re-evaluated on retry, so a
-- retried alert reflects the configuration at delivery time rather than a
-- snapshot of it.

BEGIN;

CREATE TABLE notification_outbox (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- ON DELETE CASCADE: a pending alert for a monitor that no longer exists
    -- has nothing to say. NULL is allowed for events that carry no monitor.
    monitor_id bigint REFERENCES monitors (id) ON DELETE CASCADE,
    -- kind discriminates payload. Every event the dispatcher can be handed goes
    -- through here, not just state changes: a certificate-expiry warning or a
    -- maintenance notice dropped by a full queue is just as lost.
    kind       text        NOT NULL,
    payload    jsonb       NOT NULL,
    -- attempts counts CLAIMS, not deliveries: it is incremented when a worker
    -- takes the row, so a handler that panics still advances it and the row
    -- cannot be retried forever.
    attempts   integer     NOT NULL DEFAULT 0,
    -- claimed_until is the lease, and it is set ON INSERT, not left NULL.
    --
    -- A row inserted unleased is immediately due, so the sweeper could take an
    -- event that the process which recorded it is still holding in its queue —
    -- and deliver it a second time. The writer therefore claims its own row up
    -- front: the lease says "someone is on this until then", which on insert is
    -- true. If that process dies, the lease lapses and the row becomes due,
    -- which is exactly the recovery this table exists for.
    claimed_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The worker's only query is "rows whose lease has lapsed, oldest first".
CREATE INDEX notification_outbox_due_idx ON notification_outbox (claimed_until, id);

COMMIT;

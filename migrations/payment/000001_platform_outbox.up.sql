-- Transactional outbox. Every service that publishes domain events owns a copy
-- of this table so an event and the row it describes commit together.
--
-- Retention: published rows are pruned by the nightly maintenance job after 7
-- days. They are kept that long purely so an operator can answer "did we
-- actually emit that event?" during an incident.

CREATE TABLE IF NOT EXISTS outbox_events (
    id               UUID PRIMARY KEY,
    subject          TEXT        NOT NULL,
    aggregate_id     TEXT,
    producer         TEXT        NOT NULL,
    payload          JSONB       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT         NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ,
    last_error       TEXT
);

-- The relay only ever scans unpublished rows, so the index is partial. On a
-- healthy system this index stays near-empty regardless of total table size.
CREATE INDEX IF NOT EXISTS idx_outbox_unpublished
    ON outbox_events (created_at)
    WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_outbox_aggregate
    ON outbox_events (aggregate_id, created_at DESC);

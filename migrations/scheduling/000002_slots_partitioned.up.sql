-- Materialised slot table: the heart of the MIT slot engine.
--
-- PARTITIONING NOTE (read docs/DESIGN.md §2 before changing anything here).
-- Postgres requires every UNIQUE / PRIMARY KEY constraint on a partitioned
-- table to contain the partition key. The source documentation specifies
--
--     id UUID PRIMARY KEY, ..., UNIQUE (doctor_id, start_at)
--
-- and separately "partition by month on start_at". Those two statements cannot
-- both hold: `PRIMARY KEY (id)` is rejected outright once PARTITION BY RANGE
-- (start_at) is added. The resolution used here:
--
--   * PRIMARY KEY (id, start_at) -- start_at is functionally dependent on the
--     row, so this is the same key with the partition column appended.
--   * UNIQUE (doctor_id, start_at) survives unchanged, and is *globally*
--     unique despite being enforced per-partition: two rows sharing a start_at
--     necessarily land in the same partition, so a local index is sufficient.
--   * Global uniqueness of `id` alone is not expressible. It is guaranteed by
--     the generator (UUIDv4, 122 bits of entropy) rather than by the database.
--
CREATE TABLE IF NOT EXISTS slots (
    id             UUID        NOT NULL DEFAULT gen_random_uuid(),
    doctor_id      UUID        NOT NULL,
    start_at       TIMESTAMPTZ NOT NULL,
    end_at         TIMESTAMPTZ NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'AVAILABLE',
    appointment_id UUID,

    -- Waitlist promotion parks a slot here for five minutes. The reservation
    -- lives in Postgres, not only in Redis, because a Redis failover must not
    -- hand the slot to somebody else mid-offer (ADR-007).
    reserved_for   UUID,
    reserved_until TIMESTAMPTZ,

    version        INT         NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT slots_pkey            PRIMARY KEY (id, start_at),
    CONSTRAINT slots_doctor_start_uq UNIQUE (doctor_id, start_at),
    CONSTRAINT slots_status_chk      CHECK (status IN ('AVAILABLE', 'BOOKED', 'BLOCKED', 'CANCELLED')),
    CONSTRAINT slots_range_chk       CHECK (end_at > start_at),
    -- A BOOKED slot without an appointment id is a corrupted booking; refuse it
    -- at the storage layer rather than discovering it during an incident.
    CONSTRAINT slots_booked_chk      CHECK (status <> 'BOOKED' OR appointment_id IS NOT NULL)
) PARTITION BY RANGE (start_at);

-- The search index the doctor-availability endpoint lives on. Partial, so it
-- only ever holds bookable rows: on a mature system the vast majority of slots
-- are BOOKED or archived and never enter this index.
CREATE INDEX IF NOT EXISTS idx_slots_available
    ON slots (doctor_id, start_at)
    WHERE status = 'AVAILABLE';

-- Booking arrives with a bare slot id. A non-unique index on a partitioned
-- table does not have to contain the partition key, so this is legal; the
-- planner probes one small b-tree per partition. See docs/DESIGN.md §2.3.
CREATE INDEX IF NOT EXISTS idx_slots_id ON slots (id);

-- Drives the reservation sweeper and the archival job. (status, start_at)
-- rather than the documentation's (status) alone: status has four values and
-- is useless as a leading column on its own.
CREATE INDEX IF NOT EXISTS idx_slots_status_start ON slots (status, start_at);

CREATE INDEX IF NOT EXISTS idx_slots_reserved_until
    ON slots (reserved_until)
    WHERE reserved_until IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Partition management
-- ---------------------------------------------------------------------------
--
-- TIMEZONE HAZARD: partition bounds are TIMESTAMPTZ. A bound written as the
-- bare literal '2026-08-01' is resolved against the *session* TimeZone, so the
-- same migration run by a psql session in Asia/Colombo and by the service in
-- UTC would produce partitions whose boundaries differ by 5h30m -- silently,
-- and only detectable months later as a gap. Every bound below is stamped
-- explicitly in UTC.

CREATE OR REPLACE FUNCTION create_slot_partition(p_month DATE)
RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    v_start TIMESTAMPTZ;
    v_end   TIMESTAMPTZ;
    v_name  TEXT;
BEGIN
    -- Force UTC for the duration of this function so the caller's session
    -- timezone cannot shift a partition boundary.
    SET LOCAL TimeZone = 'UTC';

    v_start := date_trunc('month', p_month::timestamptz);
    v_end   := v_start + INTERVAL '1 month';
    v_name  := format('slots_%s', to_char(v_start, 'YYYY_MM'));

    IF to_regclass(format('svc_scheduling.%I', v_name)) IS NOT NULL THEN
        RETURN v_name;
    END IF;

    -- Schema-qualified on both sides. An unqualified CREATE lands in whatever
    -- schema happens to be first on the caller's search_path, so a session
    -- that set `public, svc_scheduling` would quietly build the partition in
    -- the wrong schema and the to_regclass check above would never find it --
    -- producing a fresh partition attempt every single call.
    EXECUTE format(
        'CREATE TABLE svc_scheduling.%I PARTITION OF svc_scheduling.slots FOR VALUES FROM (%L) TO (%L)',
        v_name,
        to_char(v_start, 'YYYY-MM-DD HH24:MI:SSOF'),
        to_char(v_end,   'YYYY-MM-DD HH24:MI:SSOF')
    );
    RETURN v_name;
END;
$$;

COMMENT ON FUNCTION create_slot_partition(DATE) IS
    'Create the monthly slots partition containing p_month. Idempotent.';

-- Create p_months consecutive monthly partitions starting at the month
-- containing p_from. Returns the names it created or found, so the maintenance
-- job can log exactly what it did.
CREATE OR REPLACE FUNCTION create_slot_partitions(p_from DATE, p_months INT)
RETURNS SETOF TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    i INT;
BEGIN
    IF p_months < 1 THEN
        RAISE EXCEPTION 'create_slot_partitions: p_months must be >= 1, got %', p_months;
    END IF;
    FOR i IN 0 .. p_months - 1 LOOP
        RETURN NEXT create_slot_partition((date_trunc('month', p_from::timestamp) + (i || ' months')::interval)::date);
    END LOOP;
END;
$$;

COMMENT ON FUNCTION create_slot_partitions(DATE, INT) IS
    'Create p_months monthly slots partitions from p_from onward. Idempotent.';

-- ---------------------------------------------------------------------------
-- Archive
-- ---------------------------------------------------------------------------
-- Slots older than 90 days move here. Not partitioned: it is append-only, read
-- by analytics, and never on a request path.
CREATE TABLE IF NOT EXISTS slots_archive (
    LIKE slots INCLUDING DEFAULTS,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_slots_archive_doctor_start
    ON slots_archive (doctor_id, start_at);

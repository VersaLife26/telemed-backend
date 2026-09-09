-- Analytics projections and materialized views.
--
-- The V2 docs' revenue_daily definition joins straight off payment-service's
-- `payments` table ("SUM(p.amount) ... FROM payments p"). That table lives in
-- telemed_payment, a different logical database, and per ADR-004 this service
-- has no cross-database join available even if it wanted one. We replace the
-- join with an event-fed projection: this service consumes payment.* and
-- appointment.* domain events (already declared in
-- internal/platform/events/events.go) into its own narrow, append-style
-- projection tables, and the materialized views are built on top of those
-- local tables instead. Dashboards read the materialized views; they never
-- scan the projection tables directly, and the projection tables are never
-- scanned on a request path either -- both are refreshed/queried on a cron.

CREATE TABLE payments_projection (
    payment_id      UUID PRIMARY KEY,
    event_id        UUID        NOT NULL UNIQUE, -- last event applied; makes consumption idempotent
    appointment_id  UUID,
    doctor_id       UUID,
    patient_id      UUID,
    specialty_code  TEXT,
    district        TEXT,
    amount_cents    BIGINT      NOT NULL,
    commission_cents BIGINT     NOT NULL DEFAULT 0,
    currency        TEXT        NOT NULL DEFAULT 'LKR',
    status          TEXT        NOT NULL CHECK (status IN ('succeeded', 'failed', 'refunded')),
    provider        TEXT,
    occurred_at     TIMESTAMPTZ NOT NULL,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_payments_projection_occurred ON payments_projection (occurred_at);
CREATE INDEX idx_payments_projection_doctor ON payments_projection (doctor_id);
CREATE INDEX idx_payments_projection_status ON payments_projection (status);

CREATE TABLE appointments_projection (
    appointment_id  UUID PRIMARY KEY,
    event_id        UUID        NOT NULL,
    doctor_id       UUID,
    patient_id      UUID,
    specialty_code  TEXT,
    district        TEXT,
    status          TEXT        NOT NULL
                        CHECK (status IN ('created', 'confirmed', 'cancelled', 'completed', 'no_show')),
    scheduled_at    TIMESTAMPTZ,
    occurred_at     TIMESTAMPTZ NOT NULL,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_appointments_projection_occurred ON appointments_projection (occurred_at);
CREATE INDEX idx_appointments_projection_doctor ON appointments_projection (doctor_id);
CREATE INDEX idx_appointments_projection_district ON appointments_projection (district);
CREATE INDEX idx_appointments_projection_status ON appointments_projection (status);

-- ---------------------------------------------------------------------------
-- Materialized views. Every one has a UNIQUE index, which
-- REFRESH MATERIALIZED VIEW CONCURRENTLY requires -- without it the hourly
-- refresh job falls back to failing outright rather than silently taking the
-- exclusive lock CONCURRENTLY exists to avoid.
-- ---------------------------------------------------------------------------

CREATE MATERIALIZED VIEW revenue_daily AS
SELECT
    date_trunc('day', occurred_at) AS day,
    currency,
    SUM(amount_cents)              AS gross_cents,
    SUM(commission_cents)          AS commission_cents,
    COUNT(*)                       AS payment_count
FROM payments_projection
WHERE status = 'succeeded'
GROUP BY 1, 2
WITH DATA;

CREATE UNIQUE INDEX idx_revenue_daily_pk ON revenue_daily (day, currency);

CREATE MATERIALIZED VIEW bookings_daily AS
SELECT
    date_trunc('day', occurred_at) AS day,
    COALESCE(specialty_code, 'unknown') AS specialty_code,
    status,
    COUNT(*)                       AS booking_count
FROM appointments_projection
GROUP BY 1, 2, 3
WITH DATA;

CREATE UNIQUE INDEX idx_bookings_daily_pk ON bookings_daily (day, specialty_code, status);

CREATE MATERIALIZED VIEW doctor_utilization_daily AS
SELECT
    date_trunc('day', occurred_at)                          AS day,
    doctor_id,
    COUNT(*) FILTER (WHERE status = 'completed')             AS completed_count,
    COUNT(*) FILTER (WHERE status = 'no_show')                AS no_show_count,
    COUNT(*) FILTER (WHERE status = 'cancelled')               AS cancelled_count,
    COUNT(*)                                                   AS total_count
FROM appointments_projection
WHERE doctor_id IS NOT NULL
GROUP BY 1, 2
WITH DATA;

CREATE UNIQUE INDEX idx_doctor_utilization_daily_pk ON doctor_utilization_daily (day, doctor_id);

CREATE MATERIALIZED VIEW district_activity_daily AS
SELECT
    date_trunc('day', occurred_at) AS day,
    district,
    COUNT(*)                       AS booking_count
FROM appointments_projection
WHERE district IS NOT NULL
GROUP BY 1, 2
WITH DATA;

CREATE UNIQUE INDEX idx_district_activity_daily_pk ON district_activity_daily (day, district);

GRANT SELECT, INSERT, UPDATE ON payments_projection TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON appointments_projection TO telemed_admin_app;
GRANT SELECT ON revenue_daily TO telemed_admin_app;
GRANT SELECT ON bookings_daily TO telemed_admin_app;
GRANT SELECT ON doctor_utilization_daily TO telemed_admin_app;
GRANT SELECT ON district_activity_daily TO telemed_admin_app;

-- REFRESH MATERIALIZED VIEW is one of the few DDL-adjacent statements
-- Postgres restricts to the object's OWNER -- a GRANT cannot confer it. The
-- hourly refresh job runs as telemed_admin_app, so something has to bridge
-- that gap.
--
-- The obvious bridge, and the one this migration used to take, is
-- `ALTER MATERIALIZED VIEW ... OWNER TO telemed_admin_app`. That is the wrong
-- trade (security review F14): ownership is not a "may refresh" privilege, it
-- is every privilege. An app role that owns these views can also DROP them,
-- ALTER them, and re-GRANT anything a later migration revokes -- and once one
-- object is owned by the runtime role, "the app owns nothing" stops being an
-- invariant a reviewer can check at a glance. It also breaks outright the
-- moment migrations stop running as a superuser, because ALTER ... OWNER TO
-- requires the executing role to be able to SET ROLE to the new owner.
--
-- Instead: the views stay owned by the migration role, and refreshing them is
-- exposed as one narrow SECURITY DEFINER function. The app role may call it
-- and can do nothing else to these views. search_path is pinned, which is the
-- standard requirement for a SECURITY DEFINER function -- without it a caller
-- could prepend a schema of their own and have the function resolve
-- `revenue_daily` to an object they control.
CREATE OR REPLACE FUNCTION refresh_analytics_views() RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY public.revenue_daily;
    REFRESH MATERIALIZED VIEW CONCURRENTLY public.bookings_daily;
    REFRESH MATERIALIZED VIEW CONCURRENTLY public.doctor_utilization_daily;
    REFRESH MATERIALIZED VIEW CONCURRENTLY public.district_activity_daily;
END;
$$;

COMMENT ON FUNCTION refresh_analytics_views() IS
    'Hourly analytics refresh. SECURITY DEFINER so telemed_admin_app can refresh '
    'the materialized views without owning them (owning them would also let it '
    'DROP and ALTER them). Called by internal/analytics/repository.go RefreshAll.';

-- EXECUTE on a new function is granted to PUBLIC by default, which for a
-- SECURITY DEFINER function means "anyone who can connect". Revoke first,
-- then grant to exactly the one role that needs it.
REVOKE ALL ON FUNCTION refresh_analytics_views() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION refresh_analytics_views() TO telemed_admin_app;

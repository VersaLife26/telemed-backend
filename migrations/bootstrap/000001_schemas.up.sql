-- Bootstrap: one database, one schema per domain.
--
-- WHY SCHEMAS AND NOT ONE FLAT NAMESPACE
-- The eight stateful domains own 93 tables between them and six names collide:
--
--   specialties               svc_doctor keys it by code VARCHAR(50) and seeds
--                             Sinhala and Tamil names; svc_admin keys it by
--                             uuid with an `active` flag. Different tables.
--   working_hours             svc_doctor has an FK to doctors and no soft
--                             delete; svc_scheduling has version + deleted_at
--                             and no FK.
--   doctor_schedule_settings  svc_doctor defaults to 30-minute slots and keeps
--                             buffer_minutes NULLABLE on purpose -- NULL means
--                             "no preference", 0 means "back-to-back".
--                             svc_scheduling defaults to 15 and NOT NULL 5,
--                             which collapses that distinction.
--   drugs                     svc_admin's is the editable content catalogue;
--                             svc_record's serves prescriptions.
--   outbox_events             one per domain by design -- the relay workers
--                             and their publish ordering are per-domain.
--   consumed_events           per-consumer idempotency.
--
-- Flattening these into one namespace is a DATA MODEL change that alters
-- behaviour, not a consolidation. It is deliberately not done here. See
-- CONSOLIDATION.md; unifying them is filed as separate follow-up work with its
-- own migration and its own tests.
--
-- WHY THE svc_ PREFIX
-- `user` is a reserved word in SQL. A bare `CREATE SCHEMA user` is legal only
-- quoted, and then every later reference has to stay quoted too -- one missed
-- pair of quotes and the statement silently means CURRENT_USER instead. The
-- prefix removes the trap for every domain, not just that one.
--
-- This file runs ONCE, as the migration superuser, before any domain's own
-- migrations. Each domain then migrates with search_path = svc_<domain>,public.

CREATE SCHEMA IF NOT EXISTS svc_user;
CREATE SCHEMA IF NOT EXISTS svc_doctor;
CREATE SCHEMA IF NOT EXISTS svc_scheduling;
CREATE SCHEMA IF NOT EXISTS svc_consultation;
CREATE SCHEMA IF NOT EXISTS svc_payment;
CREATE SCHEMA IF NOT EXISTS svc_notification;
CREATE SCHEMA IF NOT EXISTS svc_record;
CREATE SCHEMA IF NOT EXISTS svc_admin;

-- Shared extensions live in public, which stays on every domain's search_path.
-- pgcrypto is created here rather than inside svc_user and svc_admin (where
-- their own migrations ask for it) so exactly one copy exists and both those
-- CREATE EXTENSION IF NOT EXISTS statements become no-ops -- an extension is a
-- database-global object, so a second CREATE in another schema would fail
-- rather than duplicate.
CREATE EXTENSION IF NOT EXISTS pgcrypto SCHEMA public;

-- Nothing is granted here. Per-domain roles and their grants are the subject of
-- 000002_roles.up.sql, which is where the compensating control for the
-- per-database least-privilege roles that a single binary cannot keep lives.

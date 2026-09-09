-- Drops every domain schema and everything in it. This is destructive and
-- exists so the bootstrap is reversible in a test harness, not because anyone
-- should run it against data they want to keep.
--
-- pgcrypto is deliberately NOT dropped: it is a database-global object that
-- other schemas may depend on, and re-creating it is cheap.
DROP SCHEMA IF EXISTS svc_admin CASCADE;
DROP SCHEMA IF EXISTS svc_record CASCADE;
DROP SCHEMA IF EXISTS svc_notification CASCADE;
DROP SCHEMA IF EXISTS svc_payment CASCADE;
DROP SCHEMA IF EXISTS svc_consultation CASCADE;
DROP SCHEMA IF EXISTS svc_scheduling CASCADE;
DROP SCHEMA IF EXISTS svc_doctor CASCADE;
DROP SCHEMA IF EXISTS svc_user CASCADE;

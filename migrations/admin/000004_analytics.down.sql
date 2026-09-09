DROP FUNCTION IF EXISTS refresh_analytics_views();

DROP MATERIALIZED VIEW IF EXISTS district_activity_daily;
DROP MATERIALIZED VIEW IF EXISTS doctor_utilization_daily;
DROP MATERIALIZED VIEW IF EXISTS bookings_daily;
DROP MATERIALIZED VIEW IF EXISTS revenue_daily;

DROP TABLE IF EXISTS appointments_projection;
DROP TABLE IF EXISTS payments_projection;

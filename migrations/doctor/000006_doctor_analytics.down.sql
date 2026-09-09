-- Reverse of 000006_doctor_analytics.up.sql, rollups before facts so the
-- dependency order reads the same way it does in the up migration.
DROP TABLE IF EXISTS doctor_peak_hours;
DROP TABLE IF EXISTS doctor_analytics_daily;
DROP TABLE IF EXISTS doctor_payout_fact;
DROP TABLE IF EXISTS doctor_payment_fact;
DROP TABLE IF EXISTS doctor_consultation_fact;
DROP TABLE IF EXISTS doctor_appointment_fact;

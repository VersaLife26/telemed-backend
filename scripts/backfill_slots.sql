-- scripts/backfill_slots.sql
-- Backfill and ensure slots for doctors (specifically Dr. Nimal Perera and all active doctors).
-- Run with: psql "$DATABASE_URL" -f scripts/backfill_slots.sql

BEGIN;

-- 1. Ensure Dr. Nimal Perera (00acaa28-fe9c-f86f-58ac-90fc876756af) has complete working hours
-- Including Saturday (6) and Sunday (0), plus morning and evening shifts.

-- Ensure schedule settings exist in svc_doctor and svc_scheduling
INSERT INTO svc_doctor.doctor_schedule_settings (doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone)
VALUES ('00acaa28-fe9c-f86f-58ac-90fc876756af', 20, 10, 0, 'Asia/Colombo')
ON CONFLICT (doctor_id) DO UPDATE SET
    timezone = 'Asia/Colombo',
    slot_duration_minutes = 20,
    buffer_minutes = 10;

INSERT INTO svc_scheduling.doctor_schedule_settings (doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone, advance_days, is_active)
VALUES ('00acaa28-fe9c-f86f-58ac-90fc876756af', 20, 10, 24, 'Asia/Colombo', 14, true)
ON CONFLICT (doctor_id) DO UPDATE SET
    timezone = 'Asia/Colombo',
    slot_duration_minutes = 20,
    buffer_minutes = 10,
    advance_days = 14,
    is_active = true;

-- Update working hours in svc_doctor (0 = Sunday, 6 = Saturday)
DELETE FROM svc_doctor.working_hours WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af';
INSERT INTO svc_doctor.working_hours (doctor_id, day_of_week, start_time, end_time) VALUES
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '09:00', '13:00'), -- Sunday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '17:00', '21:00'), -- Sunday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '09:00', '13:00'), -- Monday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '17:00', '21:00'), -- Monday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '09:00', '13:00'), -- Tuesday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '17:00', '21:00'), -- Tuesday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '09:00', '13:00'), -- Wednesday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '17:00', '21:00'), -- Wednesday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '09:00', '13:00'), -- Thursday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '17:00', '21:00'), -- Thursday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '09:00', '13:00'), -- Friday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '17:00', '21:00'), -- Friday evening
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '09:00', '13:00'), -- Saturday morning
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '17:00', '21:00'); -- Saturday evening

-- Update working hours in svc_scheduling
DELETE FROM svc_scheduling.working_hours WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af';
INSERT INTO svc_scheduling.working_hours (doctor_id, day_of_week, start_time, end_time) VALUES
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '17:00', '21:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '17:00', '21:00');

-- 2. Materialize slots for all configured working hours for the next 14 days
WITH doctor_settings AS (
    SELECT
        d.doctor_id,
        COALESCE(s.slot_duration_minutes, 20) AS slot_duration_min,
        COALESCE(s.buffer_minutes, 10) AS buffer_min,
        COALESCE(s.timezone, 'Asia/Colombo') AS tz,
        COALESCE(s.advance_days, 14) AS adv_days
    FROM (SELECT DISTINCT doctor_id FROM svc_scheduling.working_hours) d
    LEFT JOIN svc_scheduling.doctor_schedule_settings s ON s.doctor_id = d.doctor_id
    WHERE s.is_active IS NULL OR s.is_active = true
),
days_series AS (
    SELECT
        ds.doctor_id,
        ds.slot_duration_min,
        ds.buffer_min,
        ds.tz,
        (CURRENT_DATE + offs)::date AS cal_day,
        extract(dow FROM (CURRENT_DATE + offs))::int AS dow
    FROM doctor_settings ds
    CROSS JOIN generate_series(0, 14) AS offs
),
slots_to_generate AS (
    SELECT
        d.doctor_id,
        s.slot_start,
        s.slot_start + make_interval(mins => d.slot_duration_min) AS slot_end
    FROM days_series d
    JOIN svc_scheduling.working_hours wh
        ON wh.doctor_id = d.doctor_id AND wh.day_of_week = d.dow
    CROSS JOIN LATERAL (
        SELECT (d.cal_day + wh.start_time + make_interval(mins => step_mins)) AT TIME ZONE d.tz AS slot_start
        FROM generate_series(
            0,
            (EXTRACT(EPOCH FROM (wh.end_time - wh.start_time)) / 60 - d.slot_duration_min)::int,
            (d.slot_duration_min + d.buffer_min)
        ) AS step_mins
    ) s
    LEFT JOIN svc_scheduling.holidays h
        ON (h.doctor_id = d.doctor_id OR h.doctor_id IS NULL)
        AND h.date = d.cal_day
    WHERE h.id IS NULL
      AND s.slot_start > NOW()
)
INSERT INTO svc_scheduling.slots (id, doctor_id, start_at, end_at, status)
SELECT gen_random_uuid(), doctor_id, slot_start, slot_end, 'AVAILABLE'
FROM slots_to_generate
ON CONFLICT (doctor_id, start_at) DO NOTHING;

-- 3. Populate svc_doctor slot projection so search and doctor profile summary are instantly up to date
INSERT INTO svc_doctor.doctor_slot_state (slot_id, doctor_id, start_at, slot_date, status, last_event_id, last_event_at)
SELECT s.id, s.doctor_id, s.start_at, (s.start_at AT TIME ZONE 'Asia/Colombo')::date, s.status, gen_random_uuid(), NOW()
FROM svc_scheduling.slots s
WHERE s.start_at >= NOW()
ON CONFLICT (slot_id) DO UPDATE SET
    status = EXCLUDED.status,
    updated_at = NOW();

INSERT INTO svc_doctor.doctor_availability_summary (doctor_id, date, available_slot_count, next_available_at, updated_at)
SELECT
    doctor_id,
    slot_date,
    COUNT(*) FILTER (WHERE status = 'AVAILABLE'),
    MIN(start_at) FILTER (WHERE status = 'AVAILABLE'),
    NOW()
FROM svc_doctor.doctor_slot_state
WHERE start_at >= NOW()
GROUP BY doctor_id, slot_date
ON CONFLICT (doctor_id, date) DO UPDATE SET
    available_slot_count = EXCLUDED.available_slot_count,
    next_available_at    = EXCLUDED.next_available_at,
    updated_at           = NOW();

COMMIT;

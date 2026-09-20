-- Update Dr. Nimal Perera working hours and generate slots for weekend and evening consultations.

INSERT INTO doctor_schedule_settings (doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone, advance_days, is_active)
VALUES ('00acaa28-fe9c-f86f-58ac-90fc876756af', 20, 10, 24, 'Asia/Colombo', 14, true)
ON CONFLICT (doctor_id) DO UPDATE SET
    timezone = 'Asia/Colombo',
    slot_duration_minutes = 20,
    buffer_minutes = 10,
    advance_days = 14,
    is_active = true;

DELETE FROM working_hours WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af';

INSERT INTO working_hours (doctor_id, day_of_week, start_time, end_time) VALUES
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 0, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '17:00', '23:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '09:00', '13:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 6, '17:00', '23:00')
ON CONFLICT (doctor_id, day_of_week, start_time) DO NOTHING;

-- Materialize slots for the next 14 days
WITH doctor_settings AS (
    SELECT
        doctor_id,
        COALESCE(slot_duration_minutes, 20) AS slot_duration_min,
        COALESCE(buffer_minutes, 10) AS buffer_min,
        COALESCE(timezone, 'Asia/Colombo') AS tz,
        COALESCE(advance_days, 14) AS adv_days
    FROM doctor_schedule_settings
    WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af'
),
days_series AS (
    SELECT
        ds.doctor_id,
        ds.slot_duration_min,
        ds.buffer_min,
        ds.tz,
        ((NOW() AT TIME ZONE ds.tz)::date + offs)::date AS cal_day,
        extract(dow FROM ((NOW() AT TIME ZONE ds.tz)::date + offs))::int AS dow
    FROM doctor_settings ds
    CROSS JOIN generate_series(0, 14) AS offs
),
slots_to_generate AS (
    SELECT
        d.doctor_id,
        s.slot_start,
        s.slot_start + make_interval(mins => d.slot_duration_min) AS slot_end
    FROM days_series d
    JOIN working_hours wh
        ON wh.doctor_id = d.doctor_id AND wh.day_of_week = d.dow
    CROSS JOIN LATERAL (
        SELECT (d.cal_day + wh.start_time + make_interval(mins => step_mins)) AT TIME ZONE d.tz AS slot_start
        FROM generate_series(
            0,
            (EXTRACT(EPOCH FROM (wh.end_time - wh.start_time)) / 60 - d.slot_duration_min)::int,
            (d.slot_duration_min + d.buffer_min)
        ) AS step_mins
    ) s
    LEFT JOIN holidays h
        ON (h.doctor_id = d.doctor_id OR h.doctor_id IS NULL)
        AND h.date = d.cal_day
    WHERE h.id IS NULL
      AND s.slot_start > NOW()
)
INSERT INTO slots (id, doctor_id, start_at, end_at, status)
SELECT gen_random_uuid(), doctor_id, slot_start, slot_end, 'AVAILABLE'
FROM slots_to_generate
ON CONFLICT (doctor_id, start_at) DO NOTHING;

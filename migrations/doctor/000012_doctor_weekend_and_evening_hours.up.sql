-- Optional production backfill for a specific doctor who already exists.
-- No-op when that doctor is absent (empty CI databases, other environments).

DELETE FROM working_hours
WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af'
  AND EXISTS (
      SELECT 1 FROM doctors WHERE id = '00acaa28-fe9c-f86f-58ac-90fc876756af'
  );

INSERT INTO working_hours (doctor_id, day_of_week, start_time, end_time)
SELECT d.id, v.day_of_week, v.start_time::time, v.end_time::time
FROM doctors d
CROSS JOIN (
    VALUES
        (0, '09:00', '13:00'),
        (0, '17:00', '23:00'),
        (1, '09:00', '13:00'),
        (1, '17:00', '23:00'),
        (2, '09:00', '13:00'),
        (2, '17:00', '23:00'),
        (3, '09:00', '13:00'),
        (3, '17:00', '23:00'),
        (4, '09:00', '13:00'),
        (4, '17:00', '23:00'),
        (5, '09:00', '13:00'),
        (5, '17:00', '23:00'),
        (6, '09:00', '13:00'),
        (6, '17:00', '23:00')
) AS v(day_of_week, start_time, end_time)
WHERE d.id = '00acaa28-fe9c-f86f-58ac-90fc876756af'
ON CONFLICT (doctor_id, day_of_week, start_time) DO NOTHING;

-- Revert the optional production backfill. No-op when that doctor is absent.

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
        (1, '09:00', '12:00'),
        (2, '09:00', '12:00'),
        (3, '09:00', '12:00'),
        (4, '09:00', '12:00'),
        (5, '09:00', '12:00')
) AS v(day_of_week, start_time, end_time)
WHERE d.id = '00acaa28-fe9c-f86f-58ac-90fc876756af'
ON CONFLICT (doctor_id, day_of_week, start_time) DO NOTHING;

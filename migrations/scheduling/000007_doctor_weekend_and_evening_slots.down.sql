-- Revert Dr. Nimal Perera working hours to default Mon-Fri
DELETE FROM working_hours WHERE doctor_id = '00acaa28-fe9c-f86f-58ac-90fc876756af';

INSERT INTO working_hours (doctor_id, day_of_week, start_time, end_time) VALUES
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 1, '09:00', '12:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 2, '09:00', '12:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 3, '09:00', '12:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 4, '09:00', '12:00'),
    ('00acaa28-fe9c-f86f-58ac-90fc876756af', 5, '09:00', '12:00')
ON CONFLICT (doctor_id, day_of_week, start_time) DO NOTHING;

-- Update Dr. Nimal Perera (00acaa28-fe9c-f86f-58ac-90fc876756af) with weekend and evening hours
-- so that consultations are bookable 7 days a week, including today in the evening.

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

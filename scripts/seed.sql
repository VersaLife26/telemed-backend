-- Development seed data. NOT FOR PRODUCTION.
--
-- Fake doctors, patients, family members, slots, appointments, payments,
-- consultations and reviews, written straight into every domain schema --
-- together with the projections (svc_doctor availability and analytics,
-- svc_notification.doctor_directory, svc_admin *_projection) that the running
-- platform would otherwise build from events. Nothing here goes through the
-- outbox, so nothing is published to NATS.
--
--   DATABASE_URL=postgres://... make seed
--
-- Run it after `make migrate`. It is a no-op if the seed data is already
-- present, because the slot grid is relative to CURRENT_DATE and a second run
-- on a later day would turn yesterday's AVAILABLE slots into completed
-- appointments that disagree with the slots table.
--
-- Every seeded account signs in by email with the password: Password123

\set ON_ERROR_STOP on

SELECT EXISTS (
    SELECT 1 FROM svc_user.users WHERE email = 'kavindu.silva@versalife.test'
) AS already_seeded \gset
\if :already_seeded
    \echo 'seed data already present; nothing to do'
    \quit
\endif

BEGIN;

SET LOCAL search_path = public;
SET LOCAL TimeZone = 'UTC';

CREATE FUNCTION pg_temp.seed_rand(t TEXT, m INT) RETURNS INT
LANGUAGE sql IMMUTABLE AS $$
    SELECT ((('x' || substr(md5(t), 1, 8))::bit(32)::int) & 2147483647) % m
$$;

-- ---------------------------------------------------------------------------
-- People
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_doctor (
    n           INT PRIMARY KEY,
    name        TEXT,
    email       TEXT,
    phone       TEXT,
    slmc        TEXT,
    specialty   TEXT,
    languages   TEXT[],
    experience  INT,
    fee_lkr     INT,
    district    TEXT,
    days        INT[],
    start_time  TIME,
    degree      TEXT,
    institution TEXT,
    bio         TEXT,
    id          UUID,
    user_id     UUID
) ON COMMIT DROP;

INSERT INTO seed_doctor (n, name, email, phone, slmc, specialty, languages, experience, fee_lkr, district, days, start_time, degree, institution, bio) VALUES
    (1,  'Dr. Nimal Perera',          'nimal.perera@versalife.test',        '+94770000101', 'SLMC10231', 'general_practice', '{en,si}',    14, 2000, 'colombo',    '{1,2,3,4,5}', '09:00', 'MBBS',                 'University of Colombo',     'Family physician with a focus on preventive care, hypertension and everyday illnesses for all ages.'),
    (2,  'Dr. Shanthi Rajendran',     'shanthi.rajendran@versalife.test',   '+94770000102', 'SLMC11874', 'pediatrics',       '{en,ta}',    11, 3000, 'jaffna',     '{1,3,5}',     '14:00', 'MD (Paediatrics)',     'University of Jaffna',      'Consultant paediatrician. Childhood fevers, growth and development, asthma and vaccination advice.'),
    (3,  'Dr. Kasun Jayawardena',     'kasun.jayawardena@versalife.test',   '+94770000103', 'SLMC08412', 'cardiology',       '{en,si}',    19, 5000, 'kandy',      '{2,4}',       '10:00', 'MD (Cardiology)',      'University of Peradeniya',  'Consultant cardiologist. Chest pain assessment, heart failure follow-up and cholesterol management.'),
    (4,  'Dr. Dilani Fernando',       'dilani.fernando@versalife.test',     '+94770000104', 'SLMC14520', 'dermatology',      '{en,si}',     8, 3500, 'gampaha',    '{1,2,4}',     '16:00', 'MD (Dermatology)',     'University of Kelaniya',    'Dermatologist treating acne, eczema, psoriasis, hair loss and skin allergies.'),
    (5,  'Dr. Mohamed Rizwan',        'mohamed.rizwan@versalife.test',      '+94770000105', 'SLMC12093', 'endocrinology',    '{en,si,ta}', 12, 4000, 'colombo',    '{3,6}',       '09:00', 'MD (Medicine)',        'University of Colombo',     'Endocrinologist helping patients manage diabetes, thyroid disorders and weight-related conditions.'),
    (6,  'Dr. Anusha Wickramasinghe', 'anusha.wickramasinghe@versalife.test','+94770000106', 'SLMC09768', 'obstetrics_gynae', '{en,si}',    16, 4500, 'galle',      '{1,4}',       '10:00', 'MS (Obs & Gyn)',       'University of Ruhuna',      'Consultant obstetrician and gynaecologist. Antenatal care, fertility questions and menstrual health.'),
    (7,  'Dr. Pradeep Gunasekara',    'pradeep.gunasekara@versalife.test',  '+94770000107', 'SLMC13355', 'psychiatry',       '{en,si}',    10, 4000, 'colombo',    '{2,5}',       '18:00', 'MD (Psychiatry)',      'University of Colombo',     'Psychiatrist for anxiety, depression, sleep problems and stress. Confidential evening consultations.'),
    (8,  'Dr. Lakshmi Sivakumar',     'lakshmi.sivakumar@versalife.test',   '+94770000108', 'SLMC14002', 'ent',              '{en,ta}',     9, 3000, 'batticaloa', '{3,5}',       '09:00', 'MS (Otorhinolaryngology)', 'Eastern University',    'ENT surgeon. Sinusitis, ear infections, tonsillitis, hearing and voice problems.'),
    (9,  'Dr. Ruwan Bandara',         'ruwan.bandara@versalife.test',       '+94770000109', 'SLMC07215', 'orthopedics',      '{en,si}',    21, 5000, 'kurunegala', '{0,6}',       '08:00', 'MS (Orthopaedics)',    'University of Peradeniya',  'Orthopaedic surgeon. Back and joint pain, sports injuries and fracture follow-up.'),
    (10, 'Dr. Tharushi Senanayake',   'tharushi.senanayake@versalife.test', '+94770000110', 'SLMC15631', 'nutrition',        '{en,si}',     6, 2500, 'matara',     '{1,3,5}',     '19:00', 'MSc (Clinical Nutrition)', 'University of Sri Jayewardenepura', 'Clinical nutritionist. Meal plans for diabetes, weight loss, pregnancy and sports performance.');

UPDATE seed_doctor
   SET id      = md5('seed:doctor:' || n)::uuid,
       user_id = md5('seed:doctor-user:' || n)::uuid;

CREATE TEMP TABLE seed_patient (
    n        INT PRIMARY KEY,
    name     TEXT,
    email    TEXT,
    phone    TEXT,
    language TEXT,
    district TEXT,
    id       UUID
) ON COMMIT DROP;

INSERT INTO seed_patient (n, name, email, phone, language, district) VALUES
    (1,  'Kavindu Silva',         'kavindu.silva@versalife.test',        '+94771000201', 'en', 'colombo'),
    (2,  'Sanduni Herath',        'sanduni.herath@versalife.test',       '+94771000202', 'si', 'kandy'),
    (3,  'Arjun Balasubramaniam', 'arjun.balasubramaniam@versalife.test','+94771000203', 'ta', 'jaffna'),
    (4,  'Fathima Nazeer',        'fathima.nazeer@versalife.test',       '+94771000204', 'en', 'colombo'),
    (5,  'Chamara Dissanayake',   'chamara.dissanayake@versalife.test',  '+94771000205', 'si', 'kurunegala'),
    (6,  'Nirosha Weerasinghe',   'nirosha.weerasinghe@versalife.test',  '+94771000206', 'si', 'galle'),
    (7,  'Vishnu Kumar',          'vishnu.kumar@versalife.test',         '+94771000207', 'ta', 'trincomalee'),
    (8,  'Ishara Madushani',      'ishara.madushani@versalife.test',     '+94771000208', 'si', 'matara'),
    (9,  'Dinesh Rathnayake',     'dinesh.rathnayake@versalife.test',    '+94771000209', 'en', 'gampaha'),
    (10, 'Priyanka Jeyaraj',      'priyanka.jeyaraj@versalife.test',     '+94771000210', 'ta', 'batticaloa'),
    (11, 'Hasini Abeysekara',     'hasini.abeysekara@versalife.test',    '+94771000211', 'en', 'kalutara'),
    (12, 'Suresh Pathirana',      'suresh.pathirana@versalife.test',     '+94771000212', 'si', 'anuradhapura'),
    (13, 'Amaya Karunaratne',     'amaya.karunaratne@versalife.test',    '+94771000213', 'en', 'colombo'),
    (14, 'Tharindu Wijesinghe',   'tharindu.wijesinghe@versalife.test',  '+94771000214', 'si', 'ratnapura'),
    (15, 'Roshan Mendis',         'roshan.mendis@versalife.test',        '+94771000215', 'en', 'gampaha'),
    (16, 'Gayathri Nadarajah',    'gayathri.nadarajah@versalife.test',   '+94771000216', 'ta', 'vavuniya');

UPDATE seed_patient SET id = md5('seed:patient:' || n)::uuid;

CREATE TEMP TABLE seed_family (
    owner_n  INT,
    name     TEXT,
    dob      DATE,
    relation TEXT,
    id       UUID
) ON COMMIT DROP;

INSERT INTO seed_family (owner_n, name, dob, relation) VALUES
    (2,  'Senuli Herath',       '2019-04-12', 'child'),
    (4,  'Abdul Nazeer',        '1951-11-03', 'parent'),
    (9,  'Kaveesha Rathnayake', '2021-08-27', 'child'),
    (13, 'Nuwan Karunaratne',   '1988-02-19', 'spouse');

UPDATE seed_family SET id = md5('seed:family:' || owner_n)::uuid;

-- One bcrypt hash (cost 12, matching user.passwordCost) shared by every seeded
-- account; hashing per row would add several seconds for nothing.
CREATE TEMP TABLE seed_password ON COMMIT DROP AS
SELECT crypt('Password123', gen_salt('bf', 12)) AS hash;

INSERT INTO svc_user.users (id, phone, email, name, language, role, status, password_hash, email_verified_at, created_at, updated_at)
SELECT p.id, p.phone, p.email, p.name, p.language, 'patient', 'active', pw.hash,
       now() - make_interval(days => 60 + p.n), now() - make_interval(days => 60 + p.n), now()
FROM seed_patient p CROSS JOIN seed_password pw;

INSERT INTO svc_user.users (id, phone, email, name, language, role, status, password_hash, email_verified_at, created_at, updated_at)
SELECT d.user_id, d.phone, d.email, d.name, d.languages[1], 'doctor', 'active', pw.hash,
       now() - make_interval(days => 90 + d.n), now() - make_interval(days => 90 + d.n), now()
FROM seed_doctor d CROSS JOIN seed_password pw;

INSERT INTO svc_user.family_members (id, owner_user_id, name, dob, relation)
SELECT f.id, p.id, f.name, f.dob, f.relation
FROM seed_family f JOIN seed_patient p ON p.n = f.owner_n;

-- ---------------------------------------------------------------------------
-- Doctors
-- ---------------------------------------------------------------------------

INSERT INTO svc_doctor.doctors (id, user_id, slmc_number, specialty, experience_years, fee_cents, currency,
                                display_name, languages, bio, qualifications, verification_status, verified_at,
                                created_at, updated_at)
SELECT d.id, d.user_id, d.slmc, d.specialty, d.experience, d.fee_lkr * 100, 'LKR',
       d.name, d.languages, d.bio,
       jsonb_build_array(
           jsonb_build_object('degree', 'MBBS', 'institution', d.institution, 'year', 2025 - d.experience - 2),
           jsonb_build_object('degree', d.degree, 'institution', d.institution, 'year', 2025 - d.experience + 3)
       ),
       'approved', now() - make_interval(days => 85 + d.n),
       now() - make_interval(days => 90 + d.n), now()
FROM seed_doctor d;

INSERT INTO svc_doctor.working_hours (doctor_id, day_of_week, start_time, end_time)
SELECT d.id, dow, d.start_time, d.start_time + interval '3 hours'
FROM seed_doctor d CROSS JOIN LATERAL unnest(d.days) AS dow;

INSERT INTO svc_doctor.doctor_schedule_settings (doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone)
SELECT id, 20, 10, 0, 'Asia/Colombo' FROM seed_doctor;

INSERT INTO svc_doctor.doctor_applications (id, phone, email, display_name, slmc_number, specialty, languages,
                                            experience_years, fee_cents, bio, status, created_at, updated_at)
VALUES
    (md5('seed:application:1')::uuid, '+94772000301', 'chathura.ekanayake@versalife.test', 'Dr. Chathura Ekanayake', 'SLMC16044', 'neurology',        '{en,si}', 7, 450000, 'Neurologist with an interest in migraine and epilepsy care.',     'pending', now() - interval '2 days', now() - interval '2 days'),
    (md5('seed:application:2')::uuid, '+94772000302', 'meena.thevarajah@versalife.test',   'Dr. Meena Thevarajah',   'SLMC15890', 'psychology',       '{en,ta}', 5, 300000, 'Counselling psychologist for adolescents and young adults.',      'pending', now() - interval '1 day',  now() - interval '1 day'),
    (md5('seed:application:3')::uuid, '+94772000303', 'sahan.liyanage@versalife.test',     'Dr. Sahan Liyanage',     'SLMC16211', 'general_practice', '{en,si}', 3, 150000, 'General practitioner offering after-hours online consultations.', 'pending', now() - interval '5 hours', now() - interval '5 hours');

-- ---------------------------------------------------------------------------
-- Scheduling projections of the doctors
-- ---------------------------------------------------------------------------

INSERT INTO svc_scheduling.doctor_schedule_settings (doctor_id, slot_duration_minutes, buffer_minutes, max_per_day, timezone, advance_days)
SELECT id, 20, 10, 24, 'Asia/Colombo', 14 FROM seed_doctor;

INSERT INTO svc_scheduling.working_hours (doctor_id, day_of_week, start_time, end_time)
SELECT d.id, dow, d.start_time, d.start_time + interval '3 hours'
FROM seed_doctor d CROSS JOIN LATERAL unnest(d.days) AS dow;

INSERT INTO svc_scheduling.doctor_pricing (doctor_id, specialty, fee_cents, currency, languages, status, last_event_at)
SELECT id, specialty, fee_lkr * 100, 'LKR', languages, 'approved', now() FROM seed_doctor;

-- ---------------------------------------------------------------------------
-- Slot grid: the last 30 days and the next 14, six 20-minute slots on a
-- 30-minute cadence per working block, in Asia/Colombo wall-clock time.
-- ---------------------------------------------------------------------------

-- create_slot_partitions calls create_slot_partition unqualified.
SET LOCAL search_path = svc_scheduling, public;
SELECT count(*) AS slot_partitions FROM svc_scheduling.create_slot_partitions(CURRENT_DATE - 31, 3);
SET LOCAL search_path = public;

CREATE TEMP TABLE seed_grid ON COMMIT DROP AS
SELECT md5('seed:slot:' || d.n || ':' || s.start_at)::uuid AS slot_id,
       d.id AS doctor_id, d.specialty, d.fee_lkr * 100 AS amount_cents,
       s.start_at, s.start_at + interval '20 minutes' AS end_at
FROM seed_doctor d
CROSS JOIN generate_series((CURRENT_DATE - 30)::timestamp, (CURRENT_DATE + 13)::timestamp, interval '1 day') AS cal(cal_day)
CROSS JOIN generate_series(0, 150, 30) AS offs(offset_min)
CROSS JOIN LATERAL (
    SELECT ((cal.cal_day::date + d.start_time) + make_interval(mins => offs.offset_min)) AT TIME ZONE 'Asia/Colombo' AS start_at
) s
WHERE extract(dow FROM cal.cal_day)::int = ANY (d.days);

-- ---------------------------------------------------------------------------
-- Appointments: ~30% of past slots and ~15% of future slots are booked, at
-- most one booking per patient per start time.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_appt ON COMMIT DROP AS
WITH picked AS (
    SELECT g.*, p.id AS patient_id, p.district,
           CASE WHEN pg_temp.seed_rand('family:' || g.slot_id, 4) = 0 THEN f.id END AS family_member_id,
           pg_temp.seed_rand('outcome:' || g.slot_id, 100) AS roll
    FROM seed_grid g
    JOIN seed_patient p ON p.n = 1 + pg_temp.seed_rand('patient:' || g.slot_id, 16)
    LEFT JOIN seed_family f ON f.owner_n = p.n
    WHERE pg_temp.seed_rand('booked:' || g.slot_id, 100) < CASE WHEN g.start_at < now() THEN 30 ELSE 15 END
),
deduped AS (
    SELECT DISTINCT ON (patient_id, start_at) *
    FROM picked
    ORDER BY patient_id, start_at, slot_id
),
shaped AS (
    SELECT *,
           CASE WHEN start_at >= now() THEN 'confirmed'
                WHEN roll < 80 THEN 'completed'
                WHEN roll < 90 THEN 'no_show'
                ELSE 'cancelled' END AS status,
           LEAST(now() - interval '1 hour',
                 start_at - make_interval(days => 1 + pg_temp.seed_rand('created:' || slot_id, 5))) AS created_at
    FROM deduped
)
SELECT md5('seed:appointment:' || slot_id)::uuid AS id,
       md5('seed:payment:' || slot_id)::uuid AS payment_id,
       slot_id, doctor_id, patient_id, family_member_id, district, specialty, amount_cents,
       amount_cents * 15 / 100 AS commission_cents,
       start_at, end_at, status, created_at,
       created_at + interval '10 minutes' AS confirmed_at,
       CASE WHEN status = 'completed' THEN end_at + interval '5 minutes' END AS completed_at,
       CASE WHEN status = 'no_show' THEN start_at + interval '15 minutes' END AS no_show_at,
       CASE WHEN status = 'cancelled' THEN GREATEST(created_at + interval '1 hour', start_at - interval '1 day') END AS cancelled_at,
       (ARRAY[
           'Fever and headache for three days',
           'Persistent dry cough, worse at night',
           'Follow-up on blood sugar readings',
           'Skin rash on arms that is itchy',
           'Lower back pain after lifting',
           'Trouble sleeping and feeling anxious',
           'Blood pressure review, current medication',
           'Sore throat and blocked ears',
           'Child has a runny nose and mild fever',
           'Chest tightness when climbing stairs',
           'Advice on a diet plan for weight loss',
           'Irregular periods for the last two months'
       ])[1 + pg_temp.seed_rand('symptoms:' || slot_id, 12)] AS symptoms
FROM shaped;

INSERT INTO svc_scheduling.appointments (id, patient_id, doctor_id, slot_id, slot_start_at, slot_end_at, status, intake,
                                         family_member_id, payment_id, confirmed_at, completed_at, no_show_at,
                                         cancelled_at, cancelled_by, cancelled_by_role, cancellation_reason, refund_policy,
                                         amount_cents, currency, specialty, created_at, updated_at)
SELECT id, patient_id, doctor_id, slot_id, start_at, end_at, status, jsonb_build_object('symptoms', symptoms),
       family_member_id, payment_id, confirmed_at, completed_at, no_show_at,
       cancelled_at,
       CASE WHEN status = 'cancelled' THEN patient_id END,
       CASE WHEN status = 'cancelled' THEN 'patient' END,
       CASE WHEN status = 'cancelled' THEN 'Schedule conflict' END,
       CASE WHEN status = 'cancelled' THEN 'FULL' END,
       amount_cents, 'LKR', specialty, created_at,
       COALESCE(completed_at, no_show_at, cancelled_at, confirmed_at)
FROM seed_appt;

-- Future slots all exist (booked or not); past slots exist only where a live
-- appointment still holds them. Cancelled bookings released their slot.
INSERT INTO svc_scheduling.slots (id, doctor_id, start_at, end_at, status, appointment_id)
SELECT g.slot_id, g.doctor_id, g.start_at, g.end_at,
       CASE WHEN a.id IS NULL THEN 'AVAILABLE' ELSE 'BOOKED' END, a.id
FROM seed_grid g
LEFT JOIN seed_appt a ON a.slot_id = g.slot_id AND a.status <> 'cancelled'
WHERE g.start_at >= now() OR a.id IS NOT NULL;

INSERT INTO svc_scheduling.no_show_stats (patient_id, total_appointments, no_show_count, completed_count, cancelled_count, last_no_show_at)
SELECT patient_id, count(*),
       count(*) FILTER (WHERE status = 'no_show'),
       count(*) FILTER (WHERE status = 'completed'),
       count(*) FILTER (WHERE status = 'cancelled'),
       max(no_show_at)
FROM seed_appt GROUP BY patient_id;

UPDATE svc_user.users u
   SET no_show_count = s.no_show_count
  FROM svc_scheduling.no_show_stats s
 WHERE s.patient_id = u.id AND u.id IN (SELECT id FROM seed_patient);

-- ---------------------------------------------------------------------------
-- Payments (mock provider): paid on booking, fully refunded on cancellation.
-- ---------------------------------------------------------------------------

INSERT INTO svc_payment.payments (id, appointment_id, patient_id, doctor_id, specialty, amount_cents, gross_amount_cents,
                                  currency, provider,
                                  provider_intent_id, status, commission_cents, provider_fee_cents, doctor_payout_cents,
                                  refunded_cents, refunded_commission_cents, refunded_payout_cents,
                                  idempotency_key, succeeded_at, created_at)
SELECT payment_id, id, patient_id, doctor_id, specialty, amount_cents, amount_cents, 'LKR', 'mock',
       'mock_' || left(md5('intent:' || id), 24),
       CASE WHEN status = 'cancelled' THEN 'refunded' ELSE 'succeeded' END,
       commission_cents, 0, amount_cents - commission_cents,
       CASE WHEN status = 'cancelled' THEN amount_cents ELSE 0 END,
       CASE WHEN status = 'cancelled' THEN commission_cents ELSE 0 END,
       CASE WHEN status = 'cancelled' THEN amount_cents - commission_cents ELSE 0 END,
       'seed:' || id, confirmed_at, created_at
FROM seed_appt;

INSERT INTO svc_payment.refunds (id, payment_id, amount_cents, currency, reason, policy, percent, provider_refund_id,
                                 status, idempotency_key, created_at, updated_at)
SELECT md5('seed:refund:' || slot_id)::uuid, payment_id, amount_cents, 'LKR', 'Schedule conflict', 'FULL', 100,
       'mock_re_' || left(md5('refund:' || id), 20), 'succeeded', 'seed:refund:' || id, cancelled_at, cancelled_at
FROM seed_appt WHERE status = 'cancelled';

-- ---------------------------------------------------------------------------
-- Consultations and reviews for completed appointments
-- ---------------------------------------------------------------------------

INSERT INTO svc_consultation.consultations (id, appointment_id, patient_id, doctor_id, room_name, status, scheduled_at,
                                            scheduled_end_at, started_at, ended_at, duration_seconds, end_reason,
                                            created_at, updated_at)
SELECT md5('seed:consultation:' || slot_id)::uuid, id, patient_id, doctor_id, 'seed-' || id, 'ended', start_at, end_at,
       start_at + interval '2 minutes',
       start_at + interval '2 minutes' + make_interval(secs => 600 + pg_temp.seed_rand('duration:' || id, 480)),
       600 + pg_temp.seed_rand('duration:' || id, 480), 'completed', created_at, completed_at
FROM seed_appt WHERE status = 'completed';

INSERT INTO svc_doctor.review_eligibility (appointment_id, doctor_id, patient_id, created_at)
SELECT id, doctor_id, patient_id, completed_at FROM seed_appt WHERE status = 'completed';

INSERT INTO svc_doctor.reviews (doctor_id, patient_id, appointment_id, rating, comment, created_at, updated_at)
SELECT doctor_id, patient_id, id, rating,
       CASE rating
           WHEN 5 THEN (ARRAY['Very thorough and explained everything clearly.', 'Excellent doctor, listened patiently.', 'Felt much better after the advice. Highly recommend.', 'Friendly and professional, the call was on time.'])[1 + pg_temp.seed_rand('comment:' || id, 4)]
           WHEN 4 THEN (ARRAY['Good consultation, a little rushed at the end.', 'Helpful advice and a clear prescription.', 'Knowledgeable doctor. Video quality could be better.'])[1 + pg_temp.seed_rand('comment:' || id, 3)]
           WHEN 3 THEN 'It was okay. Started about ten minutes late.'
           ELSE 'Did not feel my concerns were fully addressed.'
       END,
       completed_at + interval '3 hours', completed_at + interval '3 hours'
FROM (
    SELECT *,
           CASE WHEN roll2 < 55 THEN 5 WHEN roll2 < 85 THEN 4 WHEN roll2 < 95 THEN 3 ELSE 2 END AS rating
    FROM (SELECT *, pg_temp.seed_rand('rating:' || id, 100) AS roll2 FROM seed_appt) a
    WHERE status = 'completed' AND pg_temp.seed_rand('reviewed:' || id, 100) < 60
) r;

UPDATE svc_doctor.doctors d SET
    rating = COALESCE((SELECT round(avg(r.rating), 2) FROM svc_doctor.reviews r WHERE r.doctor_id = d.id), 0),
    review_count = (SELECT count(*) FROM svc_doctor.reviews r WHERE r.doctor_id = d.id),
    consultation_count = (SELECT count(*) FROM seed_appt a WHERE a.doctor_id = d.id AND a.status = 'completed'),
    no_show_rate = COALESCE((
        SELECT round(count(*) FILTER (WHERE a.status = 'no_show')::numeric
                     / NULLIF(count(*) FILTER (WHERE a.status IN ('completed', 'no_show')), 0), 4)
        FROM seed_appt a WHERE a.doctor_id = d.id), 0)
WHERE d.id IN (SELECT id FROM seed_doctor);

-- ---------------------------------------------------------------------------
-- svc_doctor projections: availability search and doctor analytics
-- ---------------------------------------------------------------------------

INSERT INTO svc_doctor.doctor_slot_state (slot_id, doctor_id, start_at, slot_date, status, last_event_id, last_event_at)
SELECT s.id, s.doctor_id, s.start_at, (s.start_at AT TIME ZONE 'Asia/Colombo')::date, s.status,
       md5('seed:slot-event:' || s.id)::uuid, now()
FROM svc_scheduling.slots s
WHERE s.doctor_id IN (SELECT id FROM seed_doctor) AND s.start_at >= now();

INSERT INTO svc_doctor.doctor_availability_summary (doctor_id, date, available_slot_count, next_available_at)
SELECT doctor_id, slot_date,
       count(*) FILTER (WHERE status = 'AVAILABLE'),
       min(start_at) FILTER (WHERE status = 'AVAILABLE')
FROM svc_doctor.doctor_slot_state
WHERE doctor_id IN (SELECT id FROM seed_doctor)
GROUP BY doctor_id, slot_date;

INSERT INTO svc_doctor.doctor_appointment_fact (appointment_id, doctor_id, start_at, local_date, day_of_week, hour_of_day,
                                                outcome, last_event_id, last_event_at)
SELECT id, doctor_id, start_at,
       (start_at AT TIME ZONE 'Asia/Colombo')::date,
       extract(dow FROM start_at AT TIME ZONE 'Asia/Colombo')::int,
       extract(hour FROM start_at AT TIME ZONE 'Asia/Colombo')::int,
       status, md5('seed:appointment-event:' || id)::uuid, COALESCE(completed_at, no_show_at, cancelled_at)
FROM seed_appt WHERE status IN ('completed', 'no_show', 'cancelled');

INSERT INTO svc_doctor.doctor_consultation_fact (consultation_id, doctor_id, appointment_id, ended_at, local_date,
                                                 duration_seconds, end_reason, last_event_id, last_event_at)
SELECT c.id, c.doctor_id, c.appointment_id, c.ended_at, (c.ended_at AT TIME ZONE 'Asia/Colombo')::date,
       c.duration_seconds, c.end_reason, md5('seed:consultation-event:' || c.id)::uuid, c.ended_at
FROM svc_consultation.consultations c
WHERE c.doctor_id IN (SELECT id FROM seed_doctor);

INSERT INTO svc_doctor.doctor_payment_fact (payment_id, doctor_id, appointment_id, succeeded_at, local_date,
                                            gross_cents, commission_cents, net_cents, currency, last_event_id, last_event_at)
SELECT payment_id, doctor_id, id, confirmed_at, (confirmed_at AT TIME ZONE 'Asia/Colombo')::date,
       amount_cents, commission_cents, amount_cents - commission_cents, 'LKR',
       md5('seed:payment-event:' || payment_id)::uuid, confirmed_at
FROM seed_appt WHERE status <> 'cancelled';

INSERT INTO svc_doctor.doctor_analytics_daily (doctor_id, date, completed_count, no_show_count, cancelled_count,
                                               consultation_count, consultation_seconds,
                                               gross_cents, commission_cents, net_cents, currency, payment_count)
SELECT doctor_id, date,
       sum(completed), sum(no_show), sum(cancelled), sum(consultations), sum(seconds),
       sum(gross), sum(commission), sum(net),
       CASE WHEN sum(payments) > 0 THEN 'LKR' ELSE '' END,
       sum(payments)
FROM (
    SELECT doctor_id, local_date AS date,
           (outcome = 'completed')::int AS completed, (outcome = 'no_show')::int AS no_show,
           (outcome = 'cancelled')::int AS cancelled, 0 AS consultations, 0::bigint AS seconds,
           0::bigint AS gross, 0::bigint AS commission, 0::bigint AS net, 0 AS payments
    FROM svc_doctor.doctor_appointment_fact WHERE doctor_id IN (SELECT id FROM seed_doctor)
    UNION ALL
    SELECT doctor_id, local_date, 0, 0, 0, 1, duration_seconds, 0, 0, 0, 0
    FROM svc_doctor.doctor_consultation_fact WHERE doctor_id IN (SELECT id FROM seed_doctor)
    UNION ALL
    SELECT doctor_id, local_date, 0, 0, 0, 0, 0, gross_cents, commission_cents, net_cents, 1
    FROM svc_doctor.doctor_payment_fact WHERE doctor_id IN (SELECT id FROM seed_doctor)
) x
GROUP BY doctor_id, date;

INSERT INTO svc_doctor.doctor_peak_hours (doctor_id, day_of_week, hour_of_day, booking_count,
                                          completed_count, no_show_count, cancelled_count)
SELECT doctor_id, day_of_week, hour_of_day, count(*),
       count(*) FILTER (WHERE outcome = 'completed'),
       count(*) FILTER (WHERE outcome = 'no_show'),
       count(*) FILTER (WHERE outcome = 'cancelled')
FROM svc_doctor.doctor_appointment_fact
WHERE doctor_id IN (SELECT id FROM seed_doctor)
GROUP BY doctor_id, day_of_week, hour_of_day;

-- ---------------------------------------------------------------------------
-- svc_notification and svc_admin projections
-- ---------------------------------------------------------------------------

INSERT INTO svc_notification.doctor_directory (doctor_id, user_id, full_name, email, specialty, fee_cents, currency, status)
SELECT id, user_id, name, email, specialty, fee_lkr * 100, 'LKR', 'approved' FROM seed_doctor;

INSERT INTO svc_admin.user_projection (user_id, event_id, full_name, email, phone, role, status, registered_at)
SELECT u.id, md5('seed:user-event:' || u.id)::uuid, u.name, u.email, u.phone, u.role, 'active', u.created_at
FROM svc_user.users u
WHERE u.id IN (SELECT id FROM seed_patient UNION ALL SELECT user_id FROM seed_doctor);

INSERT INTO svc_admin.doctor_projection (doctor_id, event_id, full_name, email, phone, slmc_number, years_experience,
                                         specialty_code, verification_status, registered_at, fee_cents, user_id)
SELECT d.id, md5('seed:doctor-event:' || d.id)::uuid, d.name, d.email, d.phone, d.slmc, d.experience,
       d.specialty, 'approved', now() - make_interval(days => 90 + d.n), d.fee_lkr * 100, d.user_id
FROM seed_doctor d;

INSERT INTO svc_admin.appointments_projection (appointment_id, event_id, doctor_id, patient_id, specialty_code, district,
                                               status, scheduled_at, occurred_at)
SELECT id, md5('seed:appointment-event:' || id)::uuid, doctor_id, patient_id, specialty, district,
       status, start_at, COALESCE(completed_at, no_show_at, cancelled_at, confirmed_at)
FROM seed_appt;

INSERT INTO svc_admin.payments_projection (payment_id, event_id, appointment_id, doctor_id, patient_id, specialty_code,
                                           district, amount_cents, commission_cents, currency, status, provider, occurred_at)
SELECT payment_id, md5('seed:payment-event:' || payment_id)::uuid, id, doctor_id, patient_id, specialty,
       district, amount_cents, commission_cents, 'LKR',
       CASE WHEN status = 'cancelled' THEN 'refunded' ELSE 'succeeded' END, 'mock',
       COALESCE(cancelled_at, confirmed_at)
FROM seed_appt;

COMMIT;

SELECT svc_admin.refresh_analytics_views();

SELECT
    (SELECT count(*) FROM svc_user.users WHERE email LIKE '%@versalife.test') AS users,
    (SELECT count(*) FROM svc_doctor.doctors WHERE slmc_number LIKE 'SLMC%') AS doctors,
    (SELECT count(*) FROM svc_scheduling.appointments) AS appointments,
    (SELECT count(*) FROM svc_scheduling.slots WHERE status = 'AVAILABLE' AND start_at >= now()) AS open_slots,
    (SELECT count(*) FROM svc_doctor.reviews) AS reviews;

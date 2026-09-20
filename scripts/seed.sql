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
) AS base_already_seeded \gset

SELECT EXISTS (
    SELECT 1 FROM svc_record.clinical_notes WHERE subjective <> ''
) AS doctor_features_already_seeded \gset

\if :base_already_seeded
    \echo 'Base seed data (users, doctors, slots, appointments) already present; skipping base generation.'
\else
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
    (1,  'Dr. Nimal Perera',          'nimal.perera@versalife.test',        '+94770000101', 'SLMC10231', 'general_practice', '{en,si}',    14, 2000, 'colombo',    '{0,1,2,3,4,5,6}', '09:00', 'MBBS',                 'University of Colombo',     'Family physician with a focus on preventive care, hypertension and everyday illnesses for all ages.'),
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

\endif


\if :doctor_features_already_seeded
    \echo 'Doctor feature seed data (clinical notes, prescriptions, vault, waiting room, payouts) already present; skipping doctor feature generation.'
\else
    \echo 'Seeding doctor feature data (clinical notes, prescriptions, vault documents, waiting room, live consultations, payouts)...'

BEGIN;

SET LOCAL search_path = public;
SET LOCAL TimeZone = 'UTC';

CREATE OR REPLACE FUNCTION pg_temp.seed_rand(t TEXT, m INT) RETURNS INT
LANGUAGE sql IMMUTABLE AS $$
    SELECT ((('x' || substr(md5(t), 1, 8))::bit(32)::int) & 2147483647) % m
$$;

-- ---------------------------------------------------------------------------
-- 1. Consultations for Confirmed Appointments (Scheduled, Waiting, Active)
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_confirmed_consultations ON COMMIT DROP AS
SELECT 
    md5('seed:consultation:' || a.id)::uuid AS consultation_id,
    a.id AS appointment_id,
    a.patient_id,
    a.doctor_id,
    'seed-room-' || a.id AS room_name,
    a.slot_start_at AS scheduled_at,
    a.slot_end_at AS scheduled_end_at,
    a.created_at
FROM svc_scheduling.appointments a
WHERE a.status = 'confirmed';

INSERT INTO svc_consultation.consultations (
    id, appointment_id, patient_id, doctor_id, room_name, status, scheduled_at, scheduled_end_at, created_at, updated_at
)
SELECT consultation_id, appointment_id, patient_id, doctor_id, room_name, 'scheduled', scheduled_at, scheduled_end_at, created_at, created_at
FROM seed_confirmed_consultations
ON CONFLICT (appointment_id) DO NOTHING;

-- Pick 1 appointment for Dr. Nimal Perera (Doctor 1) to be ACTIVE right now
UPDATE svc_consultation.consultations
SET status = 'active', started_at = now() - interval '12 minutes'
WHERE id = (
    SELECT consultation_id FROM seed_confirmed_consultations
    WHERE doctor_id = (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC10231')
    ORDER BY scheduled_at LIMIT 1
);

-- Pick 1 appointment for Dr. Nimal Perera (Doctor 1) to be WAITING in the virtual waiting room right now
UPDATE svc_consultation.consultations
SET status = 'waiting'
WHERE id = (
    SELECT consultation_id FROM seed_confirmed_consultations
    WHERE doctor_id = (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC10231')
    ORDER BY scheduled_at OFFSET 1 LIMIT 1
);

-- Pick 1 appointment for Dr. Shanthi Rajendran (Doctor 2) to be WAITING in the virtual waiting room right now
UPDATE svc_consultation.consultations
SET status = 'waiting'
WHERE id = (
    SELECT consultation_id FROM seed_confirmed_consultations
    WHERE doctor_id = (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC11874')
    ORDER BY scheduled_at LIMIT 1
);

-- Waiting room entries
INSERT INTO svc_consultation.waiting_room_entries (id, consultation_id, doctor_id, patient_id, entered_at, status, admitted_at, created_at, updated_at)
SELECT 
    md5('seed:wr:' || c.id)::uuid,
    c.id,
    c.doctor_id,
    c.patient_id,
    now() - interval '10 minutes',
    CASE WHEN c.status = 'active' THEN 'admitted' ELSE 'waiting' END,
    CASE WHEN c.status = 'active' THEN now() - interval '8 minutes' ELSE NULL END,
    now() - interval '10 minutes',
    now() - interval '8 minutes'
FROM svc_consultation.consultations c
WHERE c.status IN ('waiting', 'active')
ON CONFLICT (consultation_id) DO UPDATE
   SET status = EXCLUDED.status, admitted_at = EXCLUDED.admitted_at;

-- Consultation participants for active consultation
INSERT INTO svc_consultation.consultation_participants (id, consultation_id, identity, role, joined_at, created_at, updated_at)
SELECT 
    md5('seed:part:doc:' || c.id)::uuid,
    c.id,
    c.doctor_id::text,
    'doctor',
    now() - interval '8 minutes',
    now() - interval '8 minutes',
    now() - interval '8 minutes'
FROM svc_consultation.consultations c
WHERE c.status = 'active'
ON CONFLICT (consultation_id, identity) DO NOTHING;

INSERT INTO svc_consultation.consultation_participants (id, consultation_id, identity, role, joined_at, created_at, updated_at)
SELECT 
    md5('seed:part:pat:' || c.id)::uuid,
    c.id,
    c.patient_id::text,
    'patient',
    now() - interval '8 minutes',
    now() - interval '8 minutes',
    now() - interval '8 minutes'
FROM svc_consultation.consultations c
WHERE c.status = 'active'
ON CONFLICT (consultation_id, identity) DO NOTHING;

-- Consultation consents (telemedicine consent)
INSERT INTO svc_consultation.consultation_consents (id, consultation_id, user_id, consent_type, granted, granted_at, created_at, updated_at)
SELECT 
    md5('seed:consent:doc:' || c.id)::uuid,
    c.id,
    d.user_id,
    'telemedicine',
    TRUE,
    now() - interval '9 minutes',
    now() - interval '9 minutes',
    now() - interval '9 minutes'
FROM svc_consultation.consultations c
JOIN svc_doctor.doctors d ON d.id = c.doctor_id
WHERE c.status IN ('waiting', 'active')
ON CONFLICT DO NOTHING;

INSERT INTO svc_consultation.consultation_consents (id, consultation_id, user_id, consent_type, granted, granted_at, created_at, updated_at)
SELECT 
    md5('seed:consent:pat:' || c.id)::uuid,
    c.id,
    c.patient_id,
    'telemedicine',
    TRUE,
    now() - interval '9 minutes',
    now() - interval '9 minutes',
    now() - interval '9 minutes'
FROM svc_consultation.consultations c
WHERE c.status IN ('waiting', 'active')
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Treating Relationships
-- ---------------------------------------------------------------------------

INSERT INTO svc_record.treating_relationships (appointment_id, doctor_id, patient_id, started_at, ended_at, created_at, updated_at)
SELECT c.appointment_id, c.doctor_id, c.patient_id, c.started_at, c.ended_at, c.created_at, c.updated_at
FROM svc_consultation.consultations c
WHERE c.status IN ('ended', 'active')
ON CONFLICT (appointment_id) DO UPDATE
   SET started_at = EXCLUDED.started_at, ended_at = EXCLUDED.ended_at;

-- ---------------------------------------------------------------------------
-- 3. Patient Health Vault Documents & Record Shares
-- ---------------------------------------------------------------------------

INSERT INTO svc_record.documents (id, owner_user_id, uploaded_by, document_type, bucket, object_key, filename, content_type, size_bytes, checksum_sha256, scan_status, created_at, updated_at)
SELECT 
    md5('seed:doc:' || u.id || ':' || d.n)::uuid,
    u.id,
    u.id,
    d.doc_type,
    'medical-reports',
    'medical-reports/' || u.id || '/' || d.fname,
    d.fname,
    d.mime,
    d.bytes,
    md5('hash:' || u.id || ':' || d.n),
    'clean',
    now() - make_interval(days => 10 + d.n),
    now() - make_interval(days => 10 + d.n)
FROM (SELECT id, row_number() OVER (ORDER BY id) AS rn FROM svc_user.users WHERE role = 'patient') u
CROSS JOIN (
    VALUES 
    (1, 'Full Blood Count (FBC) Dengue Panel.pdf', 'report', 'application/pdf', 142050),
    (2, 'Lipid Profile & Liver Function Panel.pdf', 'report', 'application/pdf', 118400),
    (3, 'Chest X-Ray Digital Radiograph PA.jpeg',   'scan',   'image/jpeg',      1845200),
    (4, 'Fasting Blood Sugar & HbA1c Report.pdf',  'report', 'application/pdf', 96500)
) AS d(n, fname, doc_type, mime, bytes)
WHERE u.rn <= 12
ON CONFLICT (bucket, object_key) DO NOTHING;

-- Document Access Log for patient documents
INSERT INTO svc_record.document_access_log (resource_type, resource_id, owner_user_id, accessed_by, accessed_by_role, action, granted, reason, created_at)
SELECT 
    'document',
    doc.id,
    doc.owner_user_id,
    doc.uploaded_by,
    'patient',
    'upload',
    TRUE,
    'owner',
    doc.created_at
FROM svc_record.documents doc
WHERE doc.bucket = 'medical-reports'
ON CONFLICT DO NOTHING;

-- Active patient record shares with treating doctors
INSERT INTO svc_record.record_shares (id, patient_id, doctor_id, granted_by, expires_at, created_at, updated_at)
SELECT 
    md5('seed:share:' || p.id || ':' || doc.id)::uuid,
    p.id,
    doc.id,
    p.id,
    now() + interval '30 days',
    now() - interval '2 days',
    now() - interval '2 days'
FROM (SELECT id, row_number() OVER (ORDER BY id) AS rn FROM svc_user.users WHERE role = 'patient') p
CROSS JOIN (SELECT id, row_number() OVER (ORDER BY id) AS drn FROM svc_doctor.doctors) doc
WHERE p.rn <= 6 AND doc.drn <= 3
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 4. Clinical SOAP Notes & WHO ICD-10 Diagnoses
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_soap (
    symptom_key INT PRIMARY KEY,
    subj        TEXT,
    obj         TEXT,
    assess      TEXT,
    plan        TEXT,
    icd_code    TEXT,
    icd_display TEXT,
    drug1_name  TEXT,
    drug1_str   TEXT,
    drug1_form  TEXT,
    drug1_dose  TEXT,
    drug1_freq  TEXT,
    drug1_dur   INT,
    drug1_qty   INT,
    drug1_inst  TEXT,
    drug2_name  TEXT,
    drug2_str   TEXT,
    drug2_form  TEXT,
    drug2_dose  TEXT,
    drug2_freq  TEXT,
    drug2_dur   INT,
    drug2_qty   INT,
    drug2_inst  TEXT
) ON COMMIT DROP;

INSERT INTO seed_soap VALUES
(1,
 'Patient presents with 3-day history of high grade continuous fever (reaching 39 C), severe frontal headache, retro-orbital pain, generalised myalgias and severe fatigue. Mild nausea, no vomiting. Passing clear urine with normal frequency. No rash, petechiae, or mucosal bleeding.',
 'Temp 39.1 C, PR 96 bpm regular, BP 118/76 mmHg. Mild conjunctival suffusion. Oropharynx clear. Heart sounds dual, no murmurs. Abdomen soft, no hepatomegaly, no tenderness. Tourniquet test negative. Capillary refill < 2 sec.',
 'Suspected Dengue fever (early febrile phase) without warning signs.',
 '1. Urgent Full Blood Count (FBC) and Dengue NS1 antigen today. 2. Tab. Paracetamol 500mg - 1g 6-hourly for fever (Max 4g/day). Avoid all NSAIDs. 3. Oral fluid maintenance 2.5 L/day (ORS, fresh juices, soup). 4. Rest and monitor urine output. 5. Return immediately if warning signs develop (persistent vomiting, abdominal pain, bleeding). Review tomorrow with FBC.',
 'A90', 'Dengue fever [classical dengue]',
 'Paracetamol', '500mg', 'tablet', '1g', '6 hourly prn', 3, 12, 'Take for high fever. Do not exceed 8 tablets in 24 hours. Drink plenty of water.',
 'ORS', 'WHO formula', 'sachet', '1 sachet in 1L water', 'throughout the day', 3, 3, 'Dissolve 1 sachet in 1 litre of clean drinking water and sip frequently.'),

(2,
 'Complains of dry, hacking cough for 5 days, predominantly troublesome at night and disturbing sleep. Accompanied by tickly throat and mild clear rhinorrhoea. Denies fever, shortness of breath, haemoptysis, or chest pain.',
 'Afebrile (36.8 C), SpO2 99% on room air, RR 16/min. Throat mildly injected, tonsils not enlarged, no exudate. Chest auscultation: clear breath sounds bilaterally, no wheezes or crackles. Peak flow 480 L/min.',
 'Acute viral bronchitis / post-viral tussive syndrome.',
 '1. Syrup Salbutamol 2mg/5ml, 5ml tid for 5 days. 2. Tab. Cetirizine 10mg nocte for 5 days. 3. Steam inhalation bd. 4. Warm water gargles and honey with lemon. 5. Adequate hydration. Follow up in 1 week if cough fails to resolve.',
 'J00', 'Acute nasopharyngitis [common cold]',
 'Salbutamol', '2mg/5ml', 'syrup', '5ml', '8 hourly', 5, 1, 'Take three times daily after food for relief of bronchospasm.',
 'Cetirizine', '10mg', 'tablet', '10mg', 'daily at night', 5, 5, 'Take once daily before bedtime. May cause mild drowsiness.'),

(3,
 'Attending for routine 3-month review of Type 2 Diabetes Mellitus. Reports good compliance with oral medications. Home glucometer fasting readings range 120-145 mg/dL. No polyuria, polydipsia, paresthesias, or visual blurring.',
 'BP 128/80 mmHg, PR 72 bpm, BMI 27.2 kg/m2. Bilateral pedal pulses (DP/PT) palpable and full. Monofilament test: intact sensation across 10 sites on both feet. Visual inspection of feet: no calluses, ulcers, or fungal infections.',
 'Type 2 Diabetes Mellitus, sub-optimally controlled on monotherapy.',
 '1. Continue Tab. Metformin 500mg bd with meals. 2. Order HbA1c, fasting lipid profile, and spot urine ACR. 3. Dietary consultation: reduce refined carbohydrate portions, eliminate sugary beverages. 4. 30 minutes daily moderate exercise. 5. Review with lab results in 2 weeks.',
 'E11.9', 'Type 2 diabetes mellitus without complications',
 'Metformin', '500mg', 'tablet', '500mg', '12 hourly with meals', 30, 60, 'Take twice daily with or immediately after meals to reduce stomach upset.',
 'Glucophage', '500mg', 'tablet', '500mg', '12 hourly with meals', 30, 60, 'Brand alternative if generic is unavailable.'),

(4,
 'Presents with itchy, erythematous maculopapular rash over bilateral forearms and dorsal hands for past 4 days. Began after gardening with new plant fertilizers. Mild burning sensation. No facial swelling, dyspnoea, or systemic symptoms.',
 'Skin examination: confluent erythematous papules and tiny vesicles over extensor forearms and dorsum of hands. Evidence of excoriations. No bullae, weeping, or secondary bacterial infection. Palms and mucosal surfaces spared.',
 'Acute allergic contact dermatitis (likely occupational/gardening trigger).',
 '1. Betamethasone valerate 0.1% cream (Betnovate): apply thinly twice daily for 5 days, then taper. 2. Tab. Cetirizine 10mg once daily for 7 days for pruritus. 3. Apply cool compresses for acute itching. 4. Strict avoidance of suspected chemical triggers, wear protective nitrile gloves. 5. Review if symptoms worsen.',
 'L70.0', 'Acne vulgaris',
 'Betnovate', '0.1%', 'cream', 'thin layer', '12 hourly', 5, 1, 'Apply thinly over affected rash areas twice daily after washing and patting dry.',
 'Cetirizine', '10mg', 'tablet', '10mg', 'daily at night', 7, 7, 'Take once daily at night to relieve itching.'),

(5,
 'Sudden onset sharp lower back pain after lifting a heavy generator 2 days ago. Pain is exacerbated by bending forward and prolonged sitting. Pain radiates to left buttock but no radiculopathy below knee. No bowel or bladder dysfunction.',
 'Antalgic gait. Marked paraspinal lumbar muscle spasm. Lumbar flexion limited to 40 degrees by pain. Straight leg raise (SLR) negative bilaterally (> 75 degrees). Lower extremity neurological exam: tone, power (5/5), sensation, and reflexes (knee/ankle) symmetrical and normal.',
 'Acute mechanical lumbar muscle strain / lumbago.',
 '1. Tab. Paracetamol 1g tid + Tab. Diclofenac 50mg bd after meals for 5 days. 2. Cap. Omeprazole 20mg daily before breakfast. 3. Avoid prolonged bed rest; encourage gentle walking as tolerated. 4. Hot fermentation to lumbar region. 5. Proper lifting ergonomics advised. Review in 1 week.',
 'M54.5', 'Low back pain',
 'Diclofenac', '50mg', 'tablet', '50mg', '12 hourly after meals', 5, 10, 'Take twice daily after meals for acute pain and inflammation. Do not take on empty stomach.',
 'Omeprazole', '20mg', 'capsule', '20mg', 'daily before breakfast', 5, 5, 'Take once daily in the morning, 30 minutes before food for stomach protection.'),

(6,
 'Patient reports 3-week history of difficulty initiating sleep, frequent nighttime awakenings, and racing thoughts regarding work deadlines. Daytime fatigue, tension headaches, and generalised restlessness. Denies suicidal ideation or depressive symptoms.',
 'Alert, cooperative, anxious affect. Speech clear, normal rate and rhythm. BP 124/80 mmHg, PR 78 bpm. Thyroid examination normal. Neurological exam normal.',
 'Adjustment disorder with anxious mood and secondary insomnia.',
 '1. Sleep hygiene education: fixed wake-up time, caffeine cessation after 2 PM, no screen time 1 hour before bed. 2. Relaxation breathing exercises (4-7-8 method). 3. Short course mild anxiolytic for 5 days if insomnia persists. 4. Referral to clinical psychologist for CBT if no improvement in 2 weeks.',
 'F41.9', 'Anxiety disorder, unspecified',
 'Cetirizine', '10mg', 'tablet', '10mg', 'daily at night', 5, 5, 'Take once daily before bedtime for mild nighttime relaxation.',
 'Panadol', '500mg', 'tablet', '500mg', 'as needed for tension headache', 5, 10, 'Take 1-2 tablets for tension headache. Do not exceed 8 tablets in 24 hours.'),

(7,
 'Presents for routine follow-up of Essential Hypertension diagnosed 2 years ago. Adherent to daily antihypertensive. No headache, blurred vision, chest pain, palpitations, or ankle swelling. Reports walking 30 minutes 4 times weekly.',
 'BP 134/82 mmHg (right arm sitting), PR 68 bpm regular. JVP normal. Apex beat in 5th intercostal space mid-clavicular line. S1 + S2 dual, no murmurs. Lungs clear to percussion and auscultation. No peripheral oedema.',
 'Essential hypertension, well controlled on ACE inhibitor / ARB therapy.',
 '1. Continue Tab. Losartan 50mg daily in the morning. 2. Maintain home BP log (morning and evening). 3. Annual renal panel (Serum Creatinine, eGFR, electrolytes). 4. Low sodium diet (< 2g sodium/day). 5. Routine follow-up in 3 months.',
 'I10', 'Essential (primary) hypertension',
 'Losartan', '50mg', 'tablet', '50mg', 'daily in morning', 30, 30, 'Take once daily in the morning with water. Monitor blood pressure weekly.',
 'Amlodipine', '5mg', 'tablet', '5mg', 'daily at night', 30, 30, 'Take once daily at bedtime if blood pressure remains above 140/90.'),

(8,
 'Patient reports 4-day history of odynophagia (painful swallowing), bilateral ear fullness/popping, and low grade fever. Mild dry cough. No hoarseness of voice, no neck swelling.',
 'Temp 37.8 C, PR 82 bpm. Oropharynx: erythematous posterior pharyngeal wall, bilateral palatine tonsils mildly enlarged with non-follicular erythema, no purulent exudate. Otoscopy: bilateral tympanic membranes intact, mild retraction, no effusion. Non-tender anterior cervical lymphadenopathy.',
 'Acute pharyngotonsillitis with secondary Eustachian tube dysfunction.',
 '1. Tab. Amoxicillin 500mg tds for 5 days. 2. Tab. Paracetamol 500mg - 1g qds prn for throat pain. 3. Warm saline mouth gargles 4 times daily. 4. Steam inhalation to relieve Eustachian tube congestion. 5. Adequate hydration.',
 'J02.9', 'Acute pharyngitis, unspecified',
 'Amoxicillin', '500mg', 'capsule', '500mg', '8 hourly after meals', 5, 15, 'Complete the full 5-day course even if symptoms resolve earlier.',
 'Paracetamol', '500mg', 'tablet', '500mg', '6 hourly prn', 5, 12, 'Take for relief of throat pain and fever. Drink warm water.'),

(9,
 'Mother brings 4-year-old child with 2-day history of clear rhinorrhoea, sneezing, and low grade intermittent fever. Feeding reasonably well, playful between fever spikes. No stridor, rapid breathing, or chest in-drawing. Normal urine output.',
 'Alert, active child in no distress. Temp 37.6 C, RR 24/min, SpO2 99% on room air. Anterior rhinoscopy: clear watery discharge, hyperaemic nasal mucosa. Throat mildly injected, no exudates. Chest: clear vesicular breath sounds throughout, no wheezes or crackles. Abdomen soft, non-tender.',
 'Acute viral upper respiratory tract infection in preschool child.',
 '1. Syrup Paracetamol 120mg/5ml, 7.5ml 6-hourly prn for fever > 38 C. 2. Normal saline nasal drops 2 drops into each nostril before feeds and sleep. 3. Frequent fluid intake (warm water, soup, juices). 4. Red flag warning signs explained: rapid breathing, chest indrawing, poor feeding, lethargy. Review in 3 days if fever persists.',
 'J06.9', 'Acute upper respiratory infection, unspecified',
 'Paracetamol Syrup', '120mg/5ml', 'syrup', '7.5ml', '6 hourly prn', 3, 1, 'Give 7.5ml for fever over 38 C. Shake well before use.',
 'ORS', 'WHO formula', 'sachet', '1 sachet in 1L water', 'sip frequently', 3, 2, 'Ensure child remains well-hydrated with oral fluids.'),

(10,
 '56-year-old male describes retrosternal chest tightness and heaviness occurring when climbing 2 flights of stairs or walking briskly up an incline. Symptoms relieve within 5 minutes of rest. No radiation to jaw or left arm. No diaphoresis, syncope, or orthopnoea. Ex-smoker (15 pack-years).',
 'BP 138/86 mmHg, PR 76 bpm regular. BMI 28.1 kg/m2. Heart sounds S1 + S2 dual, no murmurs or gallop. Chest clear. JVP not elevated. Bilateral carotid upstrokes brisk without bruits. Peripheral pulses intact.',
 'Suspected stable angina pectoris (CCS Class II). High cardiovascular risk profile.',
 '1. Urgent 12-lead resting ECG and referral for Exercise Treadmill Test (ETT) / 2D Echocardiogram. 2. Blood work: Fasting lipid profile, HbA1c, hs-Troponin I, serum creatinine. 3. Start Tab. Atorvastatin 20mg nocte. 4. Sublingual Nitroglycerin 0.5mg prn for acute chest pain (instructions given). 5. Red flag warning: go immediately to nearest emergency department if chest pain lasts > 15 min or occurs at rest.',
 'I20.9', 'Angina pectoris, unspecified',
 'Atorvastatin', '20mg', 'tablet', '20mg', 'daily at night', 30, 30, 'Take once daily at bedtime for cardiovascular protection.',
 'Concor', '5mg', 'tablet', '5mg', 'daily in morning', 30, 30, 'Take once daily in the morning to control heart rate and blood pressure.'),

(11,
 '34-year-old patient seeking medical guidance on structured lifestyle modification and weight loss. Reports gradual 8 kg weight gain over 2 years associated with sedentary desk job and irregular meals. No history of thyroid disorder, polydipsia, or joint pain.',
 'Height 165 cm, Weight 84 kg, BMI 30.9 kg/m2 (Class I Obesity). Waist circumference 94 cm. BP 126/78 mmHg, PR 70 bpm. Acanthosis nigricans absent. Systemic examination unremarkable.',
 'Class I Obesity with elevated cardiometabolic risk.',
 '1. Personalised calorie deficit nutrition plan: target 1500 kcal/day with balanced macronutrients (45% complex carbs, 25% protein, 30% healthy fats). 2. Eliminate ultra-processed foods, added sugars, and late night snacking. 3. Physical activity prescription: 150 min/week moderate aerobic exercise (brisk walking) + 2 days resistance training. 4. Baseline metabolic screening: Fasting blood glucose, lipid profile, liver enzymes. 5. Monthly follow-up for weight and waist circumference monitoring.',
 'E66.9', 'Obesity, unspecified',
 'Folic Acid', '5mg', 'tablet', '5mg', 'daily with meals', 30, 30, 'Take once daily with food as a nutritional supplement.',
 'Calcium Carbonate', '500mg', 'tablet', '500mg', 'daily after dinner', 30, 30, 'Take once daily after dinner with water.'),

(12,
 '26-year-old nulliparous female presents with menstrual irregularity for 6 months, cycles delayed by 45 to 60 days. Complains of persistent facial acne and mild hirsutism over chin. Urine pregnancy test negative. No galactorrhoea, no significant weight fluctuation.',
 'BP 118/74 mmHg, PR 72 bpm. BMI 24.8 kg/m2. Mild hirsutism (Ferriman-Gallwey score 6). Mild facial comedonal and papular acne. Thyroid gland normal. Abdomen soft, non-tender, no masses.',
 'Oligomenorrhoea, clinical presentation suggestive of Polycystic Ovary Syndrome (PCOS).',
 '1. Transabdominal Pelvic Ultrasound (Day 3-5 of cycle or random). 2. Hormonal profile: Serum FSH, LH, Total Testosterone, DHEAS, TSH, Prolactin. 3. Fasting glucose and insulin. 4. Lifestyle advice: Low glycaemic index diet and regular physical exercise. 5. Review with ultrasound and hormonal reports for cycle regulation management.',
 'N92.6', 'Irregular menstruation, unspecified',
 'Metformin', '500mg', 'tablet', '500mg', 'daily with dinner', 30, 30, 'Take once daily with evening meal to improve insulin sensitivity.',
 'Cetirizine', '10mg', 'tablet', '10mg', 'daily at night', 10, 10, 'Take at night as needed for allergic pruritus.');

CREATE TEMP TABLE seed_note_prep ON COMMIT DROP AS
SELECT 
    md5('seed:clinical_note:' || a.id)::uuid AS note_id,
    a.id AS appointment_id,
    a.doctor_id,
    a.patient_id,
    d.display_name AS doctor_name,
    d.slmc_number AS doctor_slmc,
    s.subj, s.obj, s.assess, s.plan,
    s.icd_code, s.icd_display,
    s.drug1_name, s.drug1_str, s.drug1_form, s.drug1_dose, s.drug1_freq, s.drug1_dur, s.drug1_qty, s.drug1_inst,
    s.drug2_name, s.drug2_str, s.drug2_form, s.drug2_dose, s.drug2_freq, s.drug2_dur, s.drug2_qty, s.drug2_inst,
    a.completed_at
FROM svc_scheduling.appointments a
JOIN svc_doctor.doctors d ON d.id = a.doctor_id
JOIN seed_soap s ON s.symptom_key = (1 + pg_temp.seed_rand('soap:' || a.id, 12))
WHERE a.status = 'completed';

-- Insert finalised notes
INSERT INTO svc_record.clinical_notes (id, appointment_id, doctor_id, patient_id, subjective, objective, assessment, plan, status, finalised_at, created_at, updated_at, version)
SELECT note_id, appointment_id, doctor_id, patient_id, subj, obj, assess, plan, 'finalised', completed_at, completed_at - interval '10 minutes', completed_at, 1
FROM seed_note_prep
ON CONFLICT (appointment_id) DO NOTHING;

-- Insert diagnoses
INSERT INTO svc_record.clinical_note_diagnoses (id, note_id, code, display, is_primary, sort_order, created_at)
SELECT md5('seed:diag:' || note_id)::uuid, note_id, icd_code, icd_display, TRUE, 0, completed_at
FROM seed_note_prep
ON CONFLICT (note_id, code) DO NOTHING;

-- Insert initial revision 1 (finalisation)
INSERT INTO svc_record.clinical_note_revisions (note_id, revision, subjective, objective, assessment, plan, diagnoses, change_type, amendment_reason, changed_by, changed_by_role, created_at)
SELECT note_id, 1, subj, obj, assess, plan,
       jsonb_build_array(jsonb_build_object('code', icd_code, 'display', icd_display, 'is_primary', true)),
       'finalise', '', doctor_id, 'doctor', completed_at
FROM seed_note_prep
ON CONFLICT (note_id, revision) DO NOTHING;

-- Document access log for clinical note finalisation
INSERT INTO svc_record.document_access_log (resource_type, resource_id, owner_user_id, accessed_by, accessed_by_role, action, granted, reason, created_at)
SELECT 
    'clinical_note',
    p.note_id,
    p.patient_id,
    p.doctor_id,
    'doctor',
    'finalise',
    TRUE,
    'treating_doctor',
    p.completed_at
FROM seed_note_prep p
ON CONFLICT DO NOTHING;

-- Insert Revision 2 (Amendments) for ~10% of notes
INSERT INTO svc_record.clinical_note_revisions (note_id, revision, subjective, objective, assessment, plan, diagnoses, change_type, amendment_reason, changed_by, changed_by_role, created_at)
SELECT 
    p.note_id,
    2,
    p.subj,
    p.obj || ' Follow-up lab results reviewed: renal panel normal, liver enzymes within standard reference limits.',
    p.assess || ' Patient progress satisfactory.',
    p.plan || ' Added reminder for routine 6-month preventive health checkup.',
    jsonb_build_array(jsonb_build_object('code', p.icd_code, 'display', p.icd_display, 'is_primary', true)),
    'amend',
    'Follow-up laboratory investigation reports reviewed with patient and documented into health record.',
    p.doctor_id,
    'doctor',
    p.completed_at + interval '2 days'
FROM seed_note_prep p
WHERE pg_temp.seed_rand('amend:' || p.note_id, 100) < 12
ON CONFLICT (note_id, revision) DO NOTHING;

UPDATE svc_record.clinical_notes cn
SET version = 2,
    objective = cn.objective || ' Follow-up lab results reviewed: renal panel normal, liver enzymes within standard reference limits.',
    assessment = cn.assessment || ' Patient progress satisfactory.',
    plan = cn.plan || ' Added reminder for routine 6-month preventive health checkup.',
    updated_at = cn.finalised_at + interval '2 days'
WHERE EXISTS (
    SELECT 1 FROM svc_record.clinical_note_revisions rev
    WHERE rev.note_id = cn.id AND rev.revision = 2
);

-- Insert 1 active DRAFT note for the active consultation
INSERT INTO svc_record.clinical_notes (
    id, appointment_id, doctor_id, patient_id, subjective, objective, assessment, plan, status, created_at, updated_at, version
)
SELECT 
    md5('seed:note:active:' || c.appointment_id)::uuid,
    c.appointment_id,
    c.doctor_id,
    c.patient_id,
    'Patient complains of sharp lower back pain after lifting a heavy box 2 days ago. Pain is localized, worsens with bending forward. No radiation, no weakness or numbness in legs.',
    'BP 125/82 mmHg, PR 76 bpm. Moderate tenderness over lumbar paraspinal muscles. Straight leg raise test negative bilaterally (>80 degrees). Normal lower extremity tone, power, and reflexes.',
    'Acute mechanical lumbar muscular strain.',
    '1. Paracetamol 1g tid + Diclofenac 50mg bd after meals for 5 days. 2. Omeprazole 20mg daily before breakfast. 3. Gentle walking, hot water bag application. 4. Avoid heavy lifting. Review if pain persists.',
    'draft',
    now() - interval '5 minutes',
    now() - interval '2 minutes',
    1
FROM svc_consultation.consultations c
WHERE c.status = 'active'
ON CONFLICT (appointment_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 5. Electronic Prescriptions & Prescription Items
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_rx_prep ON COMMIT DROP AS
SELECT 
    md5('seed:prescription:' || p.appointment_id)::uuid AS rx_id,
    p.appointment_id,
    p.doctor_id,
    p.patient_id,
    p.doctor_name,
    p.doctor_slmc,
    p.completed_at,
    CASE WHEN pg_temp.seed_rand('dispensed:' || p.appointment_id, 100) < 35 THEN 'dispensed' ELSE 'issued' END AS rx_status,
    p.drug1_name, p.drug1_str, p.drug1_form, p.drug1_dose, p.drug1_freq, p.drug1_dur, p.drug1_qty, p.drug1_inst,
    p.drug2_name, p.drug2_str, p.drug2_form, p.drug2_dose, p.drug2_freq, p.drug2_dur, p.drug2_qty, p.drug2_inst
FROM seed_note_prep p
WHERE pg_temp.seed_rand('has_rx:' || p.appointment_id, 100) < 85;

INSERT INTO svc_record.prescriptions (id, appointment_id, doctor_id, patient_id, doctor_name, doctor_slmc, doctor_qualifications, issued_at, verification_hmac, status, created_at, updated_at)
SELECT 
    rx_id, appointment_id, doctor_id, patient_id, doctor_name, doctor_slmc, 'MBBS (Colombo), MD',
    completed_at - interval '5 minutes',
    encode(hmac(('v1|' || rx_id || '|' || doctor_id || '|' || patient_id)::bytea, 'seed-hmac-key'::bytea, 'sha256'), 'hex'),
    rx_status,
    completed_at - interval '5 minutes',
    completed_at - interval '5 minutes'
FROM seed_rx_prep
ON CONFLICT (appointment_id) DO NOTHING;

INSERT INTO svc_record.prescription_items (id, prescription_id, drug_name, strength, form, dosage, frequency, duration_days, quantity, instructions, is_generic, sort_order, created_at)
SELECT 
    md5('seed:item:1:' || rx_id)::uuid, rx_id, drug1_name, drug1_str, drug1_form, drug1_dose, drug1_freq, drug1_dur, drug1_qty, drug1_inst, TRUE, 0, completed_at - interval '5 minutes'
FROM seed_rx_prep
UNION ALL
SELECT 
    md5('seed:item:2:' || rx_id)::uuid, rx_id, drug2_name, drug2_str, drug2_form, drug2_dose, drug2_freq, drug2_dur, drug2_qty, drug2_inst, TRUE, 1, completed_at - interval '5 minutes'
FROM seed_rx_prep
ON CONFLICT DO NOTHING;

-- Document Access Log for prescriptions
INSERT INTO svc_record.document_access_log (resource_type, resource_id, owner_user_id, accessed_by, accessed_by_role, action, granted, reason, created_at)
SELECT 
    'prescription',
    rx.rx_id,
    rx.patient_id,
    rx.doctor_id,
    'doctor',
    'view',
    TRUE,
    'treating_doctor',
    rx.completed_at - interval '4 minutes'
FROM seed_rx_prep rx
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 6. Schedule Reschedule Requests
-- ---------------------------------------------------------------------------

INSERT INTO svc_scheduling.reschedule_requests (
    id, appointment_id, patient_id, doctor_id, original_slot_id, original_start_at, original_end_at,
    proposed_slot_id, proposed_start_at, proposed_end_at, proposed_slot_created, reason, status, decided_by, decided_by_role, decided_at, created_at, updated_at
)
SELECT 
    md5('seed:reschedule:1')::uuid,
    a.id, a.patient_id, a.doctor_id,
    a.slot_id, a.slot_start_at, a.slot_end_at,
    md5('seed:proposed:1')::uuid, a.slot_start_at + interval '2 hours', a.slot_end_at + interval '2 hours',
    TRUE,
    'Doctor called for urgent emergency hospital ward rounds. Proposing to move consultation by 2 hours.',
    'pending',
    NULL, NULL, NULL,
    now() - interval '3 hours', now() - interval '3 hours'
FROM (SELECT * FROM svc_scheduling.appointments WHERE status = 'confirmed' AND doctor_id = (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC10231') LIMIT 1) a
ON CONFLICT DO NOTHING;

INSERT INTO svc_scheduling.reschedule_requests (
    id, appointment_id, patient_id, doctor_id, original_slot_id, original_start_at, original_end_at,
    proposed_slot_id, proposed_start_at, proposed_end_at, proposed_slot_created, reason, status, decided_by, decided_by_role, decided_at, created_at, updated_at
)
SELECT 
    md5('seed:reschedule:2')::uuid,
    a.id, a.patient_id, a.doctor_id,
    a.slot_id, a.slot_start_at, a.slot_end_at,
    md5('seed:proposed:2')::uuid, a.slot_start_at + interval '1 day', a.slot_end_at + interval '1 day',
    TRUE,
    'Doctor attending university CME conference on paediatric respiratory updates. Proposing tomorrow.',
    'accepted',
    a.patient_id, 'patient', now() - interval '6 hours',
    now() - interval '12 hours', now() - interval '6 hours'
FROM (SELECT * FROM svc_scheduling.appointments WHERE status = 'confirmed' AND doctor_id = (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC11874') LIMIT 1) a
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 7. Doctor Holidays & Leave
-- ---------------------------------------------------------------------------

INSERT INTO svc_scheduling.holidays (id, doctor_id, holiday_date, reason, created_at, updated_at)
SELECT md5('seed:holiday:1')::uuid, (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC10231'), CURRENT_DATE + 7, 'Annual Leave / Family Event', now(), now()
ON CONFLICT DO NOTHING;

INSERT INTO svc_scheduling.holidays (id, doctor_id, holiday_date, reason, created_at, updated_at)
SELECT md5('seed:holiday:2')::uuid, (SELECT id FROM svc_doctor.doctors WHERE slmc_number = 'SLMC11874'), CURRENT_DATE + 10, 'Paediatric Medical Conference', now(), now()
ON CONFLICT DO NOTHING;

INSERT INTO svc_scheduling.holidays (id, doctor_id, holiday_date, reason, created_at, updated_at)
SELECT md5('seed:holiday:platform')::uuid, NULL, CURRENT_DATE + 14, 'Vap Full Moon Poya Day (Public Holiday)', now(), now()
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- 8. Doctor Payouts & Earnings Settlements
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE seed_payout_prep ON COMMIT DROP AS
SELECT 
    d.id AS doctor_id,
    d.n AS doctor_n,
    w.period_start,
    w.period_end,
    w.payout_idx,
    w.paid_at,
    w.status
FROM (SELECT id, row_number() OVER (ORDER BY id) AS n FROM svc_doctor.doctors) d
CROSS JOIN (
    VALUES 
    (1, (CURRENT_DATE - 40)::date, (CURRENT_DATE - 20)::date, now() - interval '20 days', 'paid'),
    (2, (CURRENT_DATE - 19)::date, (CURRENT_DATE - 5)::date,  now() - interval '5 days',  'paid'),
    (3, (CURRENT_DATE - 4)::date,  CURRENT_DATE::date,         NULL,                       'pending')
) AS w(payout_idx, period_start, period_end, paid_at, status);

INSERT INTO svc_payment.payouts (
    id, doctor_id, period_start, period_end, amount_cents, currency, payment_count,
    status, provider, transfer_id, idempotency_key, initiated_at, paid_at, created_at, updated_at
)
SELECT 
    md5('seed:payout:' || doctor_id || ':' || payout_idx)::uuid,
    doctor_id,
    period_start,
    period_end,
    (25000 + (doctor_n * 12500)) * 100,
    'LKR',
    10 + doctor_n,
    status,
    'mock',
    CASE WHEN status = 'paid' THEN 'TRF-MOCK-' || to_char(period_end, 'YYYYMMDD') || '-' || doctor_n ELSE '' END,
    'seed:payout:idem:' || doctor_id || ':' || payout_idx,
    paid_at - interval '2 hours',
    paid_at,
    period_end + interval '1 day',
    period_end + interval '1 day'
FROM seed_payout_prep
ON CONFLICT (idempotency_key) DO NOTHING;

INSERT INTO svc_doctor.doctor_payout_fact (
    payout_id, doctor_id, amount_cents, currency, period_start, period_end,
    transfer_id, sent_at, last_event_id, last_event_at, created_at, updated_at
)
SELECT 
    md5('seed:payout:' || doctor_id || ':' || payout_idx)::uuid,
    doctor_id,
    (25000 + (doctor_n * 12500)) * 100,
    'LKR',
    period_start,
    period_end,
    'TRF-MOCK-' || to_char(period_end, 'YYYYMMDD') || '-' || doctor_n,
    paid_at,
    md5('seed:payout-evt:' || doctor_id || ':' || payout_idx)::uuid,
    paid_at,
    paid_at,
    paid_at
FROM seed_payout_prep
WHERE status = 'paid'
ON CONFLICT (payout_id) DO NOTHING;

-- Link payments to their settlements
UPDATE svc_payment.payments p
SET payout_id = po.id
FROM svc_payment.payouts po
WHERE p.doctor_id = po.doctor_id
  AND po.status = 'paid'
  AND p.status = 'succeeded'
  AND p.succeeded_at::date >= po.period_start
  AND p.succeeded_at::date <= po.period_end;

COMMIT;

\endif

SELECT svc_admin.refresh_analytics_views();

SELECT
    (SELECT count(*) FROM svc_user.users WHERE email LIKE '%@versalife.test') AS users,
    (SELECT count(*) FROM svc_doctor.doctors WHERE slmc_number LIKE 'SLMC%') AS doctors,
    (SELECT count(*) FROM svc_scheduling.appointments) AS appointments,
    (SELECT count(*) FROM svc_consultation.consultations WHERE status = 'waiting') AS waiting_room,
    (SELECT count(*) FROM svc_consultation.consultations WHERE status = 'active') AS active_calls,
    (SELECT count(*) FROM svc_record.clinical_notes) AS clinical_notes,
    (SELECT count(*) FROM svc_record.prescriptions) AS prescriptions,
    (SELECT count(*) FROM svc_record.documents) AS patient_records,
    (SELECT count(*) FROM svc_record.record_shares) AS record_shares,
    (SELECT count(*) FROM svc_scheduling.reschedule_requests) AS reschedule_requests,
    (SELECT count(*) FROM svc_payment.payouts) AS payouts,
    (SELECT count(*) FROM svc_doctor.doctor_payout_fact) AS doctor_payout_facts;

-- Doctor service core schema: profiles, credentialing, availability
-- declarations, reviews, and the local availability projection that makes
-- search possible without a cross-service database join.
--
-- Source-doc bug fixed here: the SDD's `doctors` DDL declares
--   verification_status VARCHAR(20) DEFAULT 'pending'
--     CHECK (status IN ('pending','approved','rejected'))
-- referencing a column named `status` that the table does not define (the
-- column is `verification_status`). Postgres would reject this DDL outright.
-- Fixed below: the CHECK references `verification_status`, adds the
-- 'under_review' and 'suspended' states the verification workflow needs, and
-- is written as a named constraint so a future migration can drop/replace it
-- without guessing the auto-generated name.
--
-- Cross-service note (ADR-004): doctor_id/user_id/patient_id/appointment_id
-- columns below are bare UUIDs with no FOREIGN KEY to another service's
-- database. `slots` lives in telemed_scheduling and is never joined here --
-- see doctor_slot_state / doctor_availability_summary and docs/DESIGN.md.

-- --------------------------------------------------------------------------
-- specialties: reference table seeded with what Sri Lankan patients search.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS specialties (
    code           VARCHAR(50)  PRIMARY KEY,
    name_en        VARCHAR(100) NOT NULL,
    name_si        VARCHAR(100) NOT NULL,
    name_ta        VARCHAR(100) NOT NULL,
    display_order  INT          NOT NULL DEFAULT 0,
    is_active      BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

INSERT INTO specialties (code, name_en, name_si, name_ta, display_order) VALUES
    ('general_practice',  'General Practitioner',       'සාමාන්‍ය වෛද්‍යවරයා',        'பொது மருத்துவர்',            10),
    ('pediatrics',        'Pediatrics',                  'ළමා රෝග',                    'குழந்தை மருத்துவம்',          20),
    ('obstetrics_gynae',  'Obstetrics & Gynaecology',     'ප්‍රසව හා නාරි',              'மகப்பேறு மற்றும் மகளிர் மருத்துவம்', 30),
    ('cardiology',        'Cardiology',                   'හෘද රෝග',                    'இதயவியல்',                    40),
    ('dermatology',       'Dermatology',                  'චර්ම රෝග',                   'தோல் மருத்துவம்',             50),
    ('endocrinology',     'Endocrinology & Diabetes',     'අන්තःස්‍රාවී හා දියවැඩියා',   'நாளமில்சுரப்பியல்',           60),
    ('ent',               'ENT (Ear, Nose & Throat)',     'කන් නාक් උගුරු',             'காது மூக்கு தொண்டை',          70),
    ('psychiatry',        'Psychiatry',                   'මානසික රෝග',                 'மனநல மருத்துவம்',             80),
    ('psychology',        'Psychology & Counselling',     'මනෝ විද්‍යාව',               'உளவியல் ஆலோசனை',              90),
    ('orthopedics',       'Orthopedics',                  'අස්ථි රෝග',                  'எலும்பியல்',                  100),
    ('ophthalmology',     'Ophthalmology (Eye Care)',      'ඇස් රෝග',                    'கண் மருத்துவம்',              110),
    ('neurology',         'Neurology',                    'ස්නායු රෝග',                 'நரம்பியல்',                    120),
    ('gastroenterology',  'Gastroenterology',              'ආමාශ ආන්ත්‍ර රෝග',           'இரைப்பைக் குடலியல்',          130),
    ('nephrology',        'Nephrology',                    'වකුගඩු රෝග',                 'சிறுநீரகவியல்',                140),
    ('urology',           'Urology',                       'මුත්‍රා පද්ධති රෝග',         'சிறுநீர் பாதை மருத்துவம்',     150),
    ('pulmonology',       'Pulmonology (Chest/Lung)',      'පපුව හා පෙනහළු රෝග',         'நுரையீரலியல்',                160),
    ('general_surgery',   'General Surgery',               'සාමාන්‍ය ශල්‍යකර්ම',          'பொது அறுவை சிகிச்சை',          170),
    ('dental',            'Dental',                         'දන්ත වෛද්‍ය',                 'பல் மருத்துவம்',              180),
    ('nutrition',         'Nutrition & Dietetics',          'පෝෂණවේදය',                    'ஊட்டச்சத்து ஆலோசனை',          190)
ON CONFLICT (code) DO NOTHING;

-- --------------------------------------------------------------------------
-- doctors
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctors (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No FK to telemed_user.users(id): different service, different database
    -- (ADR-004). Uniqueness is enforced so one platform account cannot hold
    -- two doctor profiles.
    user_id               UUID NOT NULL,

    slmc_number           VARCHAR(20)  NOT NULL,
    specialty             VARCHAR(50)  NOT NULL REFERENCES specialties(code),
    sub_specialties       TEXT[]       NOT NULL DEFAULT '{}',
    experience_years      SMALLINT     NOT NULL DEFAULT 0 CHECK (experience_years >= 0),
    fee_lkr               BIGINT       NOT NULL CHECK (fee_lkr >= 0), -- cents

    -- Denormalised from the user record at registration time. A doctor's
    -- public marketplace identity is deliberately not a live join to
    -- telemed_user: it lets search run entirely inside this database and
    -- keeps the "what did a patient see when they booked" record stable even
    -- if the account name changes later.
    display_name          VARCHAR(200) NOT NULL,

    languages             TEXT[]       NOT NULL DEFAULT '{}'
                             CONSTRAINT doctors_languages_valid
                             CHECK (languages <@ ARRAY['en','si','ta']::TEXT[]),
    bio                    TEXT,
    photo_url              TEXT,
    qualifications          JSONB       NOT NULL DEFAULT '[]',

    verification_status     VARCHAR(20) NOT NULL DEFAULT 'pending'
                               CONSTRAINT doctors_verification_status_valid
                               CHECK (verification_status IN
                                 ('pending','under_review','approved','rejected','suspended')),
    rejection_reason         TEXT,
    verified_at              TIMESTAMPTZ,
    verified_by              UUID,

    -- Field-level encrypted bank payout details. Ciphertext only -- see
    -- internal/doctor/port.go Encryptor and internal/doctor/crypto.go.
    -- Plaintext bank details are never written to this column, a log line,
    -- or an HTTP response.
    bank_encrypted            TEXT,

    -- Denormalised review aggregate (search hot path -- see docs/DESIGN.md).
    -- Maintained by the review write path, never computed with AVG() at
    -- query time.
    rating                    NUMERIC(3,2) NOT NULL DEFAULT 0 CHECK (rating >= 0 AND rating <= 5),
    review_count              INT          NOT NULL DEFAULT 0 CHECK (review_count >= 0),
    consultation_count        INT          NOT NULL DEFAULT 0 CHECK (consultation_count >= 0),
    no_show_rate              NUMERIC(5,4) NOT NULL DEFAULT 0 CHECK (no_show_rate >= 0 AND no_show_rate <= 1),
    accepts_new_patients      BOOLEAN      NOT NULL DEFAULT TRUE,

    -- Full-text search over the fields patients actually search: the
    -- doctor's public name and their bio. Weighted so a name match ranks
    -- above a bio match.
    search_vector             TSVECTOR GENERATED ALWAYS AS (
                                 setweight(to_tsvector('english', coalesce(display_name, '')), 'A') ||
                                 setweight(to_tsvector('english', coalesce(bio, '')), 'B')
                               ) STORED,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at                TIMESTAMPTZ,
    version                   INT NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_doctors_slmc_number ON doctors (slmc_number) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_doctors_user_id ON doctors (user_id) WHERE deleted_at IS NULL;

-- The search hot path: approved doctors filtered by specialty. Partial index
-- keeps it small regardless of how many pending/rejected rows accumulate.
CREATE INDEX IF NOT EXISTS idx_doctors_search_specialty
    ON doctors (specialty, fee_lkr)
    WHERE verification_status = 'approved' AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_doctors_search_rating
    ON doctors (rating DESC, review_count DESC)
    WHERE verification_status = 'approved' AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_doctors_verification_status
    ON doctors (verification_status, created_at)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_doctors_search_vector ON doctors USING GIN (search_vector);
CREATE INDEX IF NOT EXISTS idx_doctors_languages ON doctors USING GIN (languages);
CREATE INDEX IF NOT EXISTS idx_doctors_sub_specialties ON doctors USING GIN (sub_specialties);

-- --------------------------------------------------------------------------
-- working_hours: doctor's declared weekly availability template. This is a
-- statement of intent ("I work Mondays 9-5"), not a slot. Actual bookable
-- slots are generated and owned by the scheduling service from this
-- declaration via slot.generated events we consume, never the reverse.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS working_hours (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id     UUID NOT NULL REFERENCES doctors(id) ON DELETE CASCADE,
    day_of_week   SMALLINT NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    start_time    TIME NOT NULL,
    end_time      TIME NOT NULL,
    is_available  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT working_hours_time_order CHECK (end_time > start_time),
    UNIQUE (doctor_id, day_of_week, start_time)
);

CREATE INDEX IF NOT EXISTS idx_working_hours_doctor ON working_hours (doctor_id);

-- --------------------------------------------------------------------------
-- doctor_documents: credential uploads. Only the MinIO object key is stored
-- here, never file bytes -- the record-service owns the bucket and presigned
-- URL flow.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_documents (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id      UUID NOT NULL REFERENCES doctors(id) ON DELETE CASCADE,
    document_type  VARCHAR(50) NOT NULL
                     CHECK (document_type IN
                       ('slmc_certificate','nic','degree_certificate','specialty_board_certificate','other')),
    object_key     TEXT NOT NULL,
    uploaded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at    TIMESTAMPTZ,
    reviewed_by    UUID,
    review_notes   TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doctor_documents_doctor ON doctor_documents (doctor_id, uploaded_at DESC);

-- --------------------------------------------------------------------------
-- reviews: one per completed appointment, enforced by UNIQUE(appointment_id).
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS reviews (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    doctor_id          UUID NOT NULL REFERENCES doctors(id) ON DELETE CASCADE,
    -- No FK: patient_id/appointment_id belong to telemed_user / telemed_scheduling.
    patient_id         UUID NOT NULL,
    appointment_id     UUID NOT NULL,
    rating             SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment            TEXT,
    is_published       BOOLEAN NOT NULL DEFAULT TRUE,
    moderation_reason  TEXT,
    moderated_at       TIMESTAMPTZ,
    moderated_by       UUID,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ,
    version            INT NOT NULL DEFAULT 0,
    UNIQUE (appointment_id)
);

CREATE INDEX IF NOT EXISTS idx_reviews_doctor_published
    ON reviews (doctor_id, created_at DESC)
    WHERE is_published = TRUE AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_reviews_patient ON reviews (patient_id, created_at DESC);

-- --------------------------------------------------------------------------
-- review_eligibility: populated when we consume appointment.completed. A
-- patient may only review a doctor for an appointment recorded here, and the
-- INSERT ... ON CONFLICT DO NOTHING on appointment_id is what makes that
-- consumer idempotent under at-least-once delivery.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS review_eligibility (
    appointment_id  UUID PRIMARY KEY,
    doctor_id       UUID NOT NULL REFERENCES doctors(id) ON DELETE CASCADE,
    patient_id      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_review_eligibility_patient_doctor
    ON review_eligibility (patient_id, doctor_id);

-- --------------------------------------------------------------------------
-- doctor_slot_state: a per-slot mirror built entirely from slot.generated /
-- slot.booked / slot.released events consumed from the scheduling service.
-- This is NOT a copy of the scheduling database and is not authoritative for
-- booking decisions -- it exists solely to answer "does this doctor have
-- availability" for search, cheaply and locally. See docs/DESIGN.md.
--
-- last_event_id / last_event_at implement last-writer-wins by event time
-- (not delivery time), which is what makes the projection converge to the
-- correct state under at-least-once, out-of-order redelivery.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_slot_state (
    slot_id        UUID PRIMARY KEY,
    doctor_id      UUID NOT NULL,
    start_at       TIMESTAMPTZ NOT NULL,
    slot_date      DATE NOT NULL,
    status         VARCHAR(20) NOT NULL CHECK (status IN ('AVAILABLE','BOOKED')),
    last_event_id  UUID NOT NULL,
    last_event_at  TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_doctor_slot_state_doctor_date ON doctor_slot_state (doctor_id, slot_date);

-- --------------------------------------------------------------------------
-- doctor_availability_summary: the projection search actually filters and
-- sorts on. Recomputed transactionally from doctor_slot_state every time a
-- slot event changes that doctor's state for that date.
-- --------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS doctor_availability_summary (
    doctor_id             UUID NOT NULL,
    date                  DATE NOT NULL,
    available_slot_count  INT NOT NULL DEFAULT 0 CHECK (available_slot_count >= 0),
    next_available_at     TIMESTAMPTZ,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (doctor_id, date)
);

-- Powers both the available=now/available_after filter and sort=next_available.
-- Partial (only rows with actual availability) keeps it tight.
CREATE INDEX IF NOT EXISTS idx_availability_summary_next_available
    ON doctor_availability_summary (doctor_id, next_available_at)
    WHERE available_slot_count > 0;

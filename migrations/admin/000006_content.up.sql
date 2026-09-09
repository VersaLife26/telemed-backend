-- Content management: specialties, symptoms, the Sri Lankan drug formulary,
-- and waiting-room educational articles. This service is the system of
-- record for all four (SDD section 8.8 places "Content" under the admin
-- service). Other services that need this reference data (doctor-service for
-- specialty search, consultation-service for symptom intake, record-service
-- for prescription drug lookup) do not query this database -- per ADR-004
-- there is no cross-database join available -- they subscribe to the
-- content.* events this service publishes via the outbox on every create or
-- update and keep their own local read copy.

CREATE TABLE specialties (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code       TEXT        NOT NULL,
    name_en    TEXT        NOT NULL,
    name_si    TEXT,
    name_ta    TEXT,
    active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    version    INT         NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX idx_specialties_code ON specialties (code) WHERE deleted_at IS NULL;

CREATE TABLE symptoms (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code             TEXT        NOT NULL,
    name_en          TEXT        NOT NULL,
    name_si          TEXT,
    name_ta          TEXT,
    specialty_codes  TEXT[]      NOT NULL DEFAULT '{}',
    active           BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,
    version          INT         NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX idx_symptoms_code ON symptoms (code) WHERE deleted_at IS NULL;

-- id, name, strength, form, manufacturer is the literal shape the SDD names
-- for the drugs table; we add the platform-standard audit columns on top.
CREATE TABLE drugs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT        NOT NULL,
    strength     TEXT,
    form         TEXT,
    manufacturer TEXT,
    active       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMPTZ,
    version      INT         NOT NULL DEFAULT 1
);
CREATE INDEX idx_drugs_name ON drugs (lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE articles (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title            TEXT        NOT NULL,
    slug             TEXT        NOT NULL,
    body             TEXT        NOT NULL,
    language         TEXT        NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'si', 'ta')),
    specialty_code   TEXT,
    published        BOOLEAN     NOT NULL DEFAULT FALSE,
    published_at     TIMESTAMPTZ,
    author_admin_id  UUID REFERENCES admin_users (id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,
    version          INT         NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX idx_articles_slug ON articles (slug) WHERE deleted_at IS NULL;
CREATE INDEX idx_articles_published ON articles (published, published_at DESC) WHERE deleted_at IS NULL;

GRANT SELECT, INSERT, UPDATE ON specialties TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON symptoms TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON drugs TO telemed_admin_app;
GRANT SELECT, INSERT, UPDATE ON articles TO telemed_admin_app;

-- Rejected applications must not block a re-apply with the same SLMC from
-- landing in the verification queue. A global unique on slmc_number did:
-- doctor-service allows reuse after reject, the projector upsert then hit
-- 23505, JetStream burned MaxDeliver, and the SoR row stayed pending forever
-- while the admin queue never saw it.
--
-- Keep uniqueness only where it matters for the queue and for live profiles:
-- at most one pending review, and at most one approved doctor, per SLMC.

DROP INDEX IF EXISTS idx_doctor_projection_slmc;

CREATE UNIQUE INDEX idx_doctor_projection_slmc_pending
    ON doctor_projection (slmc_number)
    WHERE verification_status = 'pending';

CREATE UNIQUE INDEX idx_doctor_projection_slmc_approved
    ON doctor_projection (slmc_number)
    WHERE verification_status = 'approved';

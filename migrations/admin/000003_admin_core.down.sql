DROP INDEX IF EXISTS idx_verification_checklists_status;
DROP INDEX IF EXISTS idx_verification_checklists_doctor;
DROP INDEX IF EXISTS idx_verification_checklists_doctor_pending;
DROP TABLE IF EXISTS verification_checklists;

DROP TRIGGER IF EXISTS trg_system_configs_no_delete ON system_configs;
DROP TRIGGER IF EXISTS trg_system_configs_no_update ON system_configs;
DROP FUNCTION IF EXISTS block_mutation_generic();
DROP INDEX IF EXISTS idx_system_configs_key_effective;
DROP TABLE IF EXISTS system_configs;

DROP INDEX IF EXISTS idx_dispute_comments_dispute;
DROP TABLE IF EXISTS dispute_comments;

DROP INDEX IF EXISTS idx_disputes_doctor;
DROP INDEX IF EXISTS idx_disputes_patient;
DROP INDEX IF EXISTS idx_disputes_appointment;
DROP INDEX IF EXISTS idx_disputes_assigned_to;
DROP INDEX IF EXISTS idx_disputes_status;
DROP TABLE IF EXISTS disputes;

DROP INDEX IF EXISTS idx_admin_users_role;
DROP INDEX IF EXISTS idx_admin_users_email;
DROP INDEX IF EXISTS idx_admin_users_keycloak_subject;
DROP TABLE IF EXISTS admin_users;

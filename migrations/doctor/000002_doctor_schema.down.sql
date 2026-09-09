-- Reverse of 000002_doctor_schema.up.sql, in dependency order.

DROP INDEX IF EXISTS idx_availability_summary_next_available;
DROP TABLE IF EXISTS doctor_availability_summary;

DROP INDEX IF EXISTS idx_doctor_slot_state_doctor_date;
DROP TABLE IF EXISTS doctor_slot_state;

DROP INDEX IF EXISTS idx_review_eligibility_patient_doctor;
DROP TABLE IF EXISTS review_eligibility;

DROP INDEX IF EXISTS idx_reviews_patient;
DROP INDEX IF EXISTS idx_reviews_doctor_published;
DROP TABLE IF EXISTS reviews;

DROP INDEX IF EXISTS idx_doctor_documents_doctor;
DROP TABLE IF EXISTS doctor_documents;

DROP INDEX IF EXISTS idx_working_hours_doctor;
DROP TABLE IF EXISTS working_hours;

DROP INDEX IF EXISTS idx_doctors_sub_specialties;
DROP INDEX IF EXISTS idx_doctors_languages;
DROP INDEX IF EXISTS idx_doctors_search_vector;
DROP INDEX IF EXISTS idx_doctors_verification_status;
DROP INDEX IF EXISTS idx_doctors_search_rating;
DROP INDEX IF EXISTS idx_doctors_search_specialty;
DROP INDEX IF EXISTS uq_doctors_user_id;
DROP INDEX IF EXISTS uq_doctors_slmc_number;
DROP TABLE IF EXISTS doctors;

DROP TABLE IF EXISTS specialties;

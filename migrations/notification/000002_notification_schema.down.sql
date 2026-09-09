DROP INDEX IF EXISTS idx_reminder_state_due;
DROP TABLE IF EXISTS appointment_reminder_state;

DROP INDEX IF EXISTS idx_dead_letters_unresolved;
DROP TABLE IF EXISTS notification_dead_letters;

DROP INDEX IF EXISTS idx_delivery_log_notification;
DROP TABLE IF EXISTS delivery_log;

DROP INDEX IF EXISTS uq_templates_active;
DROP TABLE IF EXISTS templates;

DROP INDEX IF EXISTS idx_device_tokens_active;
DROP TABLE IF EXISTS device_tokens;

DROP TABLE IF EXISTS notification_preferences;

DROP INDEX IF EXISTS idx_notifications_status;
DROP INDEX IF EXISTS idx_notifications_user_created;
DROP INDEX IF EXISTS idx_notifications_dispatch;
DROP TABLE IF EXISTS notifications;

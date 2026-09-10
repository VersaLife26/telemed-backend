package notification

import (
	"fmt"

	"github.com/spf13/viper"

	"telemed/internal/platform/config"
)

// Config extends the shared platform config.Base with everything this
// service needs: which provider backs each channel (ADR-002: selection is
// config-driven and per-channel) and that provider's own settings.
type Config struct {
	config.Base `mapstructure:",squash"`

	// Provider selection, one of "console" | "direct" | "novu". console
	// (the default) requires no credentials at all, which is what lets
	// `docker compose up` boot this service with nothing configured.
	SMSProvider   string `mapstructure:"notification_sms_provider"`
	PushProvider  string `mapstructure:"notification_push_provider"`
	EmailProvider string `mapstructure:"notification_email_provider"`

	// SMSBackend selects which direct SMS transport to use when
	// SMSProvider=direct: "dialog" (default, cheaper for Sri Lankan
	// numbers, requires TRCSL approval at volume) or "twilio"
	// (international fallback).
	SMSBackend string `mapstructure:"notification_sms_backend"`

	DialogBaseURL       string `mapstructure:"dialog_base_url"`
	DialogApplicationID string `mapstructure:"dialog_application_id"`
	DialogPassword      string `mapstructure:"dialog_password"`
	DialogSourceAddress string `mapstructure:"dialog_source_address"`

	TwilioAccountSID string `mapstructure:"twilio_account_sid"`
	TwilioAuthToken  string `mapstructure:"twilio_auth_token"`
	TwilioFrom       string `mapstructure:"twilio_from"`

	FCMProjectID              string `mapstructure:"fcm_project_id"`
	FCMServiceAccountJSONPath string `mapstructure:"fcm_service_account_json_path"`

	SMTPHost     string `mapstructure:"smtp_host"`
	SMTPPort     int    `mapstructure:"smtp_port"`
	SMTPUsername string `mapstructure:"smtp_username"`
	SMTPPassword string `mapstructure:"smtp_password"`
	SMTPFrom     string `mapstructure:"smtp_from"`

	NovuBaseURL       string `mapstructure:"novu_base_url"`
	NovuAPIKey        string `mapstructure:"novu_api_key"`
	NovuWorkflowSMS   string `mapstructure:"novu_workflow_sms"`
	NovuWorkflowPush  string `mapstructure:"novu_workflow_push"`
	NovuWorkflowEmail string `mapstructure:"novu_workflow_email"`

	DispatchIntervalSeconds       int `mapstructure:"dispatch_interval_seconds"`
	DispatchBatchSize             int `mapstructure:"dispatch_batch_size"`
	ReminderIntervalSeconds       int `mapstructure:"reminder_interval_seconds"`
	MaintenanceIntervalHours      int `mapstructure:"maintenance_interval_hours"`
	NotificationBodyRetentionDays int `mapstructure:"notification_body_retention_days"`
	DeviceTokenRetentionDays      int `mapstructure:"device_token_retention_days"`
	MaxSendAttempts               int `mapstructure:"max_send_attempts"`
	SendBackoffBaseSeconds        int `mapstructure:"send_backoff_base_seconds"`
	SendBackoffMaxMinutes         int `mapstructure:"send_backoff_max_minutes"`

	// UserServiceGRPCAddr is telemed-user-service's internal gRPC endpoint.
	// This service resolves a recipient's phone and email there, per
	// notification, at enqueue time -- the canonical user.registered event
	// deliberately does not broadcast them, because a phone number is
	// PHI-adjacent and that event fans out to every consumer on the platform.
	//
	// It is required: without it there is no address to send anything to, and
	// a notification service that cannot address a message is not degraded,
	// it is broken. Boot fails rather than discovering it at 3am.
	UserServiceGRPCAddr string `mapstructure:"user_service_grpc_addr"`
	// UserLookupTimeoutSeconds bounds one directory call.
	UserLookupTimeoutSeconds int `mapstructure:"user_lookup_timeout_seconds"`

	// The gRPC hop to user-service carries the recipient's phone number and
	// email address -- the very fields user.registered deliberately does not
	// broadcast. See GRPCTLSConfig for why plaintext is still the default and
	// why production has to say so out loud.
	UserServiceGRPCTLS            bool   `mapstructure:"user_service_grpc_tls"`
	UserServiceGRPCCAFile         string `mapstructure:"user_service_grpc_ca_file"`
	UserServiceGRPCServerName     string `mapstructure:"user_service_grpc_server_name"`
	UserServiceGRPCAllowPlaintext bool   `mapstructure:"user_service_grpc_allow_plaintext"`

	// The service account this process authenticates AS on the mesh. Its
	// Keycloak service account must hold the "service" realm role, because
	// user-service authenticates every gRPC method and exempts none -- so a
	// client without a token cannot resolve a single recipient.
	MeshClientID     string `mapstructure:"mesh_client_id"`
	MeshClientSecret string `mapstructure:"mesh_client_secret"`
	// MeshTokenURL is the realm token endpoint. Derived from
	// KeycloakBaseURL/KeycloakRealm when empty.
	MeshTokenURL string `mapstructure:"mesh_token_url"`
	// MeshStaticToken is a pre-minted service token used instead of the OAuth
	// client-credentials flow, for a deployment with no identity provider on
	// the mesh path. See servicetoken.Config.StaticToken.
	MeshStaticToken   string `mapstructure:"mesh_static_token"`
	KeycloakBaseURL   string `mapstructure:"keycloak_base_url"`
	KeycloakRealmName string `mapstructure:"keycloak_realm"`

	// PublicAppBaseURL is the origin patients open, e.g.
	// https://app.yourapp.lk. Every deep link in a notification body (join a
	// consultation, download a prescription, view a receipt) is built from it
	// -- see links.go for why those are not carried on the events.
	PublicAppBaseURL string `mapstructure:"public_app_base_url"`

	// DoctorPortalBaseURL is where approved applicants complete OTP
	// (e.g. https://doctor.zecool.cf). Falls back to PublicAppBaseURL when empty.
	DoctorPortalBaseURL string `mapstructure:"doctor_portal_base_url"`

	// DoctorApplicationsNotifyEmail receives ops mail when a doctor applies.
	// Empty skips the ops email (admin in-app notification still works).
	DoctorApplicationsNotifyEmail string `mapstructure:"doctor_applications_notify_email"`

	// WebhookSecret authenticates POST /webhooks/delivery/{provider}.
	//
	// Delivery callbacks carry no platform JWT. Every backend this service
	// uses (Twilio status callbacks, Dialog DLRs, FCM, SES via SNS) lets the
	// callback URL be configured, so a high-entropy token in that URL -- or in
	// the X-Telemed-Webhook-Token header -- is how a caller that cannot sign
	// is authenticated. Without it the route is an unauthenticated write to
	// any notification row found by provider_message_id, which is a Twilio SID
	// rather than a secret.
	WebhookSecret string `mapstructure:"notification_webhook_secret"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// this service believes. Requests from anywhere else have their client
	// address taken from the TCP peer, which no header can forge. Empty means
	// the private ranges only.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// Validate extends config.Base.Validate with this service's own required
// keys.
func (c Config) Validate() error {
	if err := c.Base.Validate(); err != nil {
		return err
	}
	if c.UserServiceGRPCAddr == "" {
		return fmt.Errorf("config: USER_SERVICE_GRPC_ADDR is required " +
			"(recipients' phone and email are resolved from user-service; the events do not carry them)")
	}
	if c.PublicAppBaseURL == "" {
		return fmt.Errorf("config: PUBLIC_APP_BASE_URL is required " +
			"(every join/receipt/prescription link in a message body is built from it)")
	}
	// Deliberately a hard requirement rather than "required in prod". An
	// unauthenticated delivery webhook is an unauthenticated write to any
	// notification row; there is no environment in which the right answer is
	// to leave it open, and a dev default would be published and therefore
	// worthless.
	if len(c.WebhookSecret) < minWebhookSecretLen {
		return fmt.Errorf("config: NOTIFICATION_WEBHOOK_SECRET is required and must be at least %d characters "+
			"(it is the only thing authenticating POST /webhooks/delivery/{provider}, which updates notification "+
			"rows found by a provider message id -- a Twilio SID, not a secret)", minWebhookSecretLen)
	}
	return nil
}

// minWebhookSecretLen is long enough that guessing is hopeless and short
// enough to paste into a provider console.
const minWebhookSecretLen = 24

// RegisterDefaults sets defaults and binds every environment key Config adds
// on top of config.Base. Call it on the viper instance returned by
// config.New before Unmarshal.
func RegisterDefaults(v *viper.Viper) {
	v.SetDefault("notification_sms_provider", "console")
	v.SetDefault("notification_push_provider", "console")
	v.SetDefault("notification_email_provider", "console")
	v.SetDefault("notification_sms_backend", "dialog")

	v.SetDefault("smtp_port", 587)

	v.SetDefault("dispatch_interval_seconds", 2)
	v.SetDefault("dispatch_batch_size", 50)
	v.SetDefault("reminder_interval_seconds", 60)
	v.SetDefault("maintenance_interval_hours", 24)
	v.SetDefault("notification_body_retention_days", 90)
	v.SetDefault("device_token_retention_days", 30)
	v.SetDefault("user_lookup_timeout_seconds", 3)
	v.SetDefault("user_service_grpc_addr", "localhost:9091")
	v.SetDefault("public_app_base_url", "http://localhost:3000")
	v.SetDefault("mesh_client_id", "telemed-api")
	v.SetDefault("keycloak_realm", "telemedicine")

	v.SetDefault("max_send_attempts", 6)
	v.SetDefault("send_backoff_base_seconds", 30)
	v.SetDefault("send_backoff_max_minutes", 30)

	for _, k := range []string{
		"notification_sms_provider", "notification_push_provider", "notification_email_provider",
		"notification_sms_backend",
		"dialog_base_url", "dialog_application_id", "dialog_password", "dialog_source_address",
		"twilio_account_sid", "twilio_auth_token", "twilio_from",
		"fcm_project_id", "fcm_service_account_json_path",
		"smtp_host", "smtp_port", "smtp_username", "smtp_password", "smtp_from",
		"novu_base_url", "novu_api_key", "novu_workflow_sms", "novu_workflow_push", "novu_workflow_email",
		"dispatch_interval_seconds", "dispatch_batch_size", "reminder_interval_seconds",
		"maintenance_interval_hours", "notification_body_retention_days", "device_token_retention_days",
		"max_send_attempts", "send_backoff_base_seconds", "send_backoff_max_minutes",
		"user_service_grpc_addr", "user_lookup_timeout_seconds", "public_app_base_url",
		"doctor_portal_base_url", "doctor_applications_notify_email",
		"user_service_grpc_tls", "user_service_grpc_ca_file",
		"user_service_grpc_server_name", "user_service_grpc_allow_plaintext",
		"notification_webhook_secret",
		"mesh_client_id", "mesh_client_secret", "mesh_token_url", "mesh_static_token",
		"keycloak_base_url", "keycloak_realm",
		"trusted_proxies",
	} {
		_ = v.BindEnv(k)
	}
}

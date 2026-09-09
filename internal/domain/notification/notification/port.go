package notification

import (
	"context"
	"errors"
)

// Message is everything a NotificationProvider needs to actually place a
// call to FCM, Dialog, Twilio, SMTP or Novu. It is deliberately provider-
// agnostic: no field here is FCM- or Twilio-shaped.
type Message struct {
	Channel Channel

	// Recipient identifies where the message goes. Exactly one of these is
	// set, matching Channel: To for sms/email, DeviceToken for push. in_app
	// has no external recipient at all -- the InAppProvider never reads this.
	To          string // E.164 phone for sms, email address for email
	DeviceToken string // FCM/APNs/web-push token for push

	Subject string // used by email as the subject line, by push as the title
	Body    string // sms body, push body, email HTML body

	// Locale travels with the message so a provider that itself renders
	// content (Novu workflows) can pick the right language, and so a direct
	// provider can pick a sender ID/from-address per language if configured.
	Locale Locale

	// TemplateKey and NotificationID are for provider-side logging/metrics
	// correlation only; providers must not use them to look up content --
	// Body/Subject are already fully rendered.
	TemplateKey    TemplateKey
	NotificationID string
}

// Receipt is what a provider hands back after accepting (or rejecting) a
// send. ProviderMessageID lets a later delivery webhook correlate back to
// this attempt.
type Receipt struct {
	ProviderMessageID string
	// Delivered is true only for providers that can confirm delivery
	// synchronously (the console provider, in effect). Everything else
	// starts at "sent" and moves to "delivered" only if a webhook says so.
	Delivered bool
}

// NotificationProvider is the interface every notification backend
// implements: the direct FCM/Dialog/Twilio/SMTP provider (the default, no
// Novu required), the Novu-backed provider, and the console provider used
// in local development. Business logic in service.go never imports an SDK
// directly -- see ADR-002 and AGENT-BRIEF §0.6.
type NotificationProvider interface {
	// Send delivers one already-rendered message. A permanent failure (bad
	// phone number, unregistered token, invalid address) and a transient one
	// (provider 503, timeout) must both be reported as errors; the caller
	// classifies which is which via Classify, not the provider itself,
	// because classification rules differ per transport, not per vendor.
	Send(ctx context.Context, msg Message) (Receipt, error)
	// Channels lists which channels this provider instance can carry. A
	// direct provider registered only for SMS returns []Channel{ChannelSMS}.
	Channels() []Channel
	// Name identifies the provider in notifications.provider and metrics
	// labels, e.g. "direct-fcm", "direct-dialog", "novu", "console".
	Name() string
}

// ProviderRegistry resolves which NotificationProvider instance handles a
// given channel. Selection is config-driven and per-channel (ADR-002): sms
// might run through the direct Dialog/Twilio provider while push and email
// run through Novu, or everything can run through console in local dev.
type ProviderRegistry interface {
	ProviderFor(ch Channel) (NotificationProvider, bool)
}

// permanentError and transientError let a provider (or the classifier) tag
// an error with its class explicitly. Classify falls back to heuristics only
// when a caller has not already tagged the error this way.
type classifiedError struct {
	class ErrorClass
	err   error
}

func (c *classifiedError) Error() string { return c.err.Error() }
func (c *classifiedError) Unwrap() error { return c.err }

// Permanent wraps err so Classify reports it as permanent regardless of its
// message text: the provider already knows (e.g. FCM returned
// UNREGISTERED), so there is no need to guess from a string.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{class: ErrorClassPermanent, err: err}
}

// Transient wraps err so Classify reports it as transient.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{class: ErrorClassTransient, err: err}
}

// Classify determines whether err should be retried. An explicitly tagged
// error (Permanent/Transient) wins; otherwise Classify falls back to a
// generic transient default, because assuming an unrecognised error is
// retryable is the safer failure mode -- a wrongly-retried permanent error
// costs a few wasted attempts, but a wrongly-abandoned transient error costs
// a notification the patient never gets.
func Classify(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var ce *classifiedError
	if errors.As(err, &ce) {
		return ce.class
	}
	return ErrorClassTransient
}

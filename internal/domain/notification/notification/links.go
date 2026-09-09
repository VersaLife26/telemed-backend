package notification

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Links builds the deep links that appear in notification bodies.
//
// These used to arrive as join_link / download_url / receipt_url fields on
// the events themselves, which meant five different producers each had to
// know this service's URL scheme and remember to send it -- and the one that
// forgot produced a message with a bare "" where a link should be, with
// nothing logged. A deep link is a presentation concern: it is derived from
// an id this service already has plus one configured base URL, so it belongs
// here and not on the wire.
type Links struct {
	// BaseURL is the public origin patients open, e.g. https://app.yourapp.lk.
	// Trailing slashes are trimmed on construction.
	BaseURL string
}

// NewLinks normalises the configured base URL.
func NewLinks(baseURL string) Links {
	return Links{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/")}
}

// Join is the link a patient taps to enter a consultation.
func (l Links) Join(appointmentID uuid.UUID) string {
	return l.path("/appointments/%s/join", appointmentID)
}

// Prescription is where a patient downloads an issued e-prescription.
func (l Links) Prescription(prescriptionID uuid.UUID) string {
	return l.path("/prescriptions/%s", prescriptionID)
}

// Receipt is where a patient views a payment receipt.
func (l Links) Receipt(paymentID uuid.UUID) string {
	return l.path("/payments/%s/receipt", paymentID)
}

// path returns "" rather than a relative URL when no base is configured. An
// empty string in a message body is bad; a link that resolves against the
// wrong origin -- an SMS client guessing, or a mail client rewriting -- is
// worse, because it looks like it works.
func (l Links) path(format string, id uuid.UUID) string {
	if l.BaseURL == "" {
		return ""
	}
	return l.BaseURL + fmt.Sprintf(format, id)
}

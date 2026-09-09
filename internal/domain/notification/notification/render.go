package notification

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
)

// TemplateData is the variable set every seeded template draws from. Not
// every template uses every field -- reminder_1h has no use for Code, for
// instance -- text/template and html/template both leave an unused field
// alone, they only fail on a referenced field that is absent.
type TemplateData struct {
	DoctorName       string
	DateTime         string // pre-formatted in the recipient's locale/timezone
	FeeLKR           string // pre-formatted, e.g. "Rs. 2,500.00"
	AmountLKR        string
	Reason           string
	DownloadURL      string
	ReceiptURL       string
	JoinLink         string
	Code             string
	ExpiresInMinutes int
	ApplicantEmail   string
	Phone            string
	SLMCNumber       string
	Specialty        string
	PortalURL        string
}

// Rendered is the subject/body pair produced by rendering a Template against
// TemplateData.
type Rendered struct {
	Subject string
	Body    string
}

// RenderTemplate renders t against data using the parser appropriate to the
// channel: html/template for email, which auto-escapes every interpolated
// value into an HTML-safe context, and text/template for sms/push/in_app,
// which are plain text end to end. Using text/template for an HTML body is
// exactly the injection hole AGENT-BRIEF calls out -- a doctor's rejection
// Reason or a cancellation Reason free-text field could otherwise carry
// "<script>" straight into a patient's inbox.
func RenderTemplate(t Template, data TemplateData) (Rendered, error) {
	if t.Channel == ChannelEmail {
		return renderHTML(t, data)
	}
	return renderText(t, data)
}

func renderText(t Template, data TemplateData) (Rendered, error) {
	var out Rendered

	if t.SubjectTemplate != "" {
		subj, err := renderTextString("subject", t.SubjectTemplate, data)
		if err != nil {
			return out, err
		}
		out.Subject = subj
	}

	body, err := renderTextString("body", t.BodyTemplate, data)
	if err != nil {
		return out, err
	}
	out.Body = body
	return out, nil
}

func renderTextString(name, tmpl string, data TemplateData) (string, error) {
	parsed, err := texttemplate.New(name).Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("notification: parse %s template: %w", name, err)
	}
	var buf bytes.Buffer
	if err := parsed.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("notification: execute %s template: %w", name, err)
	}
	return buf.String(), nil
}

func renderHTML(t Template, data TemplateData) (Rendered, error) {
	var out Rendered

	if t.SubjectTemplate != "" {
		// The subject line is plain text even for email (it is never
		// rendered as HTML by a mail client), so it still goes through
		// text/template -- an HTML-escaped subject would show a patient
		// literal "&amp;" in their inbox.
		subj, err := renderTextString("email-subject", t.SubjectTemplate, data)
		if err != nil {
			return out, err
		}
		out.Subject = subj
	}

	parsed, err := htmltemplate.New("email-body").Option("missingkey=zero").Parse(t.BodyTemplate)
	if err != nil {
		return out, fmt.Errorf("notification: parse email body template: %w", err)
	}
	var buf bytes.Buffer
	if err := parsed.Execute(&buf, data); err != nil {
		return out, fmt.Errorf("notification: execute email body template: %w", err)
	}
	out.Body = buf.String()
	return out, nil
}

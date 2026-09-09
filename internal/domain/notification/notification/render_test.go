package notification

import (
	"strings"
	"testing"
)

// bookingConfirmedData is representative real data -- a Sri Lankan doctor
// name, a formatted LKR fee, a formatted local date/time -- exercised
// against every locale/channel combination the seed migration ships.
func bookingConfirmedData() TemplateData {
	return TemplateData{
		DoctorName: "Priyanka Fernando",
		DateTime:   "21 Aug 2026, 14:30",
		FeeLKR:     "Rs. 2,500.00",
	}
}

func TestRenderTemplate_AllLocales_SMS(t *testing.T) {
	// The Sinhala rows spell the yansaya ligature in ".../\u200d..." rather
	// than with a literal zero-width joiner. The bytes are identical; the
	// escape is there so an invisible character cannot be dropped by an editor
	// or a copy-paste and silently change what a patient's SMS says.
	cases := []struct {
		locale Locale
		body   string
		want   []string
	}{
		{LocaleEnglish, "Your appointment with Dr. {{.DoctorName}} is confirmed for {{.DateTime}}. Fee: {{.FeeLKR}}.",
			[]string{"Priyanka Fernando", "21 Aug 2026, 14:30", "Rs. 2,500.00"}},
		{LocaleSinhala, "ඔබගේ වෛද්\u200dය {{.DoctorName}} හමුව {{.DateTime}} සඳහා තහවුරු කර ඇත. ගාස්තුව: {{.FeeLKR}}.",
			[]string{"Priyanka Fernando", "21 Aug 2026, 14:30", "Rs. 2,500.00", "වෛද්\u200dය", "තහවුරු"}},
		{LocaleTamil, "மருத்துவர் {{.DoctorName}} உடனான உங்கள் சந்திப்பு {{.DateTime}} அன்று உறுதி செய்யப்பட்டுள்ளது. கட்டணம்: {{.FeeLKR}}.",
			[]string{"Priyanka Fernando", "21 Aug 2026, 14:30", "Rs. 2,500.00", "மருத்துவர்", "உறுதி"}},
	}

	for _, tc := range cases {
		t.Run(string(tc.locale), func(t *testing.T) {
			tmpl := Template{Key: TemplateBookingConfirmed, Channel: ChannelSMS, Locale: tc.locale, BodyTemplate: tc.body}
			rendered, err := RenderTemplate(tmpl, bookingConfirmedData())
			if err != nil {
				t.Fatalf("RenderTemplate: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(rendered.Body, want) {
					t.Errorf("rendered body %q does not contain %q", rendered.Body, want)
				}
			}
			if rendered.Subject != "" {
				t.Errorf("sms template should not render a subject, got %q", rendered.Subject)
			}
		})
	}
}

func TestRenderTemplate_EmailAutoEscapesHTML(t *testing.T) {
	// A cancellation Reason is free text that could, in principle, come from
	// an upstream doctor/admin note. html/template must neutralise it in an
	// email body; text/template (sms/push/in_app) must leave it verbatim,
	// since it is never interpreted as markup on those channels.
	maliciousReason := `<script>alert(1)</script>`

	email := Template{
		Key: TemplateAppointmentCancelled, Channel: ChannelEmail, Locale: LocaleEnglish,
		SubjectTemplate: "Your appointment has been cancelled",
		BodyTemplate:    `<p>Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled.</p><p>Reason: {{.Reason}}</p>`,
	}
	data := TemplateData{DoctorName: "Silva", DateTime: "21 Aug 2026, 09:00", Reason: maliciousReason}

	rendered, err := RenderTemplate(email, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if strings.Contains(rendered.Body, "<script>") {
		t.Fatalf("email body was not HTML-escaped, injection hole present: %s", rendered.Body)
	}
	if !strings.Contains(rendered.Body, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag in email body, got: %s", rendered.Body)
	}

	sms := Template{
		Key: TemplateAppointmentCancelled, Channel: ChannelSMS, Locale: LocaleEnglish,
		BodyTemplate: `Your appointment with Dr. {{.DoctorName}} on {{.DateTime}} has been cancelled. {{.Reason}}`,
	}
	smsRendered, err := RenderTemplate(sms, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if !strings.Contains(smsRendered.Body, maliciousReason) {
		t.Fatalf("sms body should carry the reason verbatim (plain text channel), got: %s", smsRendered.Body)
	}
}

func TestRenderTemplate_EmailSubjectIsPlainText(t *testing.T) {
	// The subject line must never be HTML-escaped -- an email client renders
	// it as plain text, so an escaped "&" would show literally in an inbox.
	email := Template{
		Key: TemplateBookingConfirmed, Channel: ChannelEmail, Locale: LocaleEnglish,
		SubjectTemplate: "Appointment with {{.DoctorName}} & clinic",
		BodyTemplate:    "<p>ok</p>",
	}
	rendered, err := RenderTemplate(email, TemplateData{DoctorName: "A&B Clinic"})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if strings.Contains(rendered.Subject, "&amp;") {
		t.Fatalf("subject was HTML-escaped, want plain text: %q", rendered.Subject)
	}
	if !strings.Contains(rendered.Subject, "A&B Clinic") {
		t.Fatalf("subject missing interpolated value: %q", rendered.Subject)
	}
}

func TestRenderTemplate_MissingFieldRendersEmpty(t *testing.T) {
	// missingkey=zero: a template referencing a TemplateData field the
	// caller left unset renders the zero value instead of erroring. This
	// matters because not every template uses every field (reminder_1h has
	// no Code, otp_code has no DoctorName).
	tmpl := Template{Channel: ChannelSMS, Locale: LocaleEnglish, BodyTemplate: "code={{.Code}} name={{.DoctorName}}"}
	rendered, err := RenderTemplate(tmpl, TemplateData{Code: "123456"})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if rendered.Body != "code=123456 name=" {
		t.Fatalf("unexpected render: %q", rendered.Body)
	}
}

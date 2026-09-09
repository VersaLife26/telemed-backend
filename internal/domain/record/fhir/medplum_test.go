package fhir

import (
	"strings"
	"testing"
)

// TestSummariseFHIRError_NeverEchoesTheServerBody is SECURITY-REVIEW F20b,
// pinned.
//
// medplum.go interpolated the whole non-2xx response body into the error it
// returned. Those errors are wrapped and logged by records.attachFHIRReference
// and prescriptions.attachFHIR next to an unmasked document_id or
// prescription_id, so whatever the FHIR server chose to echo went to the log
// sink already joined to the patient. A rejected MedicationRequest routinely
// echoes the medication text; a rejected DocumentReference echoes the
// attachment title, which on this platform is the patient's own filename --
// "jane_doe_hiv_results.pdf" is the example the platform's own MaskFilename
// helper was written for.
//
// The bodies below are the real OperationOutcome shapes a FHIR R4 server
// returns. What must survive is the failure class; what must not survive is
// one character of free text.
func TestSummariseFHIRError_NeverEchoesTheServerBody(t *testing.T) {
	// Every one of these strings is PHI. None may appear in the output.
	phi := []string{
		"Metformin", "metformin", "500mg",
		"jane_doe_hiv_results.pdf",
		"Tenofovir/Emtricitabine",
		"Sertraline",
		"E11.9",
	}

	bodies := []struct {
		name string
		body string
		want string
	}{
		{
			"MedicationRequest rejected, drug echoed in diagnostics",
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"Unknown medication code for 'Metformin 500mg (tablet)'"}]}`,
			"OperationOutcome error/invalid",
		},
		{
			"drug echoed in details.text instead",
			`{"resourceType":"OperationOutcome","issue":[{"severity":"fatal","code":"processing","details":{"text":"Cannot dispense Tenofovir/Emtricitabine to this Patient"}}]}`,
			"OperationOutcome fatal/processing",
		},
		{
			"DocumentReference rejected, filename echoed",
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"structure","diagnostics":"Attachment.title 'jane_doe_hiv_results.pdf' exceeds the configured length"}]}`,
			"OperationOutcome error/structure",
		},
		{
			"several issues are capped",
			`{"resourceType":"OperationOutcome","issue":[
				{"severity":"error","code":"invalid","diagnostics":"Sertraline"},
				{"severity":"error","code":"invalid","diagnostics":"E11.9"},
				{"severity":"error","code":"invalid","diagnostics":"Metformin"},
				{"severity":"error","code":"invalid","diagnostics":"Metformin"},
				{"severity":"error","code":"invalid","diagnostics":"Metformin"},
				{"severity":"error","code":"invalid","diagnostics":"Metformin"},
				{"severity":"warning","code":"informational","diagnostics":"Metformin"}]}`,
			"+2 more",
		},
		{
			"a server that puts free text where a code belongs gets it dropped",
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"Metformin 500mg was rejected"}]}`,
			"error/?",
		},
		{
			"an unrecognised body is described, not quoted",
			`<html><body>502 Bad Gateway: upstream said Metformin</body></html>`,
			"body suppressed",
		},
		{
			"a plain JSON error that is not an OperationOutcome is also suppressed",
			`{"error":"invalid_request","error_description":"Sertraline is not a known code"}`,
			"body suppressed",
		},
		{"an empty body", ``, "empty body"},
	}

	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseFHIRError([]byte(tc.body))
			if !strings.Contains(got, tc.want) {
				t.Errorf("summariseFHIRError() = %q, want it to contain %q", got, tc.want)
			}
			for _, secret := range phi {
				if strings.Contains(got, secret) {
					t.Errorf("summariseFHIRError() = %q -- it leaked %q from the server body", got, secret)
				}
			}
			// Nothing the server sent may pass through at all, so the
			// summary is bounded by construction rather than by the
			// server's good manners.
			if len(got) > 256 {
				t.Errorf("summariseFHIRError() produced %d bytes; a log line must be bounded regardless of what the server returns", len(got))
			}
		})
	}
}

package fhir

import "context"

// Client is the port every FHIR-aware piece of business logic depends on.
// Business code never imports an HTTP client for Medplum directly
// (AGENT-BRIEF §0.6). The default binding is NoOpClient, so a developer
// machine or a CI run with no FHIR server configured still boots and passes
// every non-FHIR test.
type Client interface {
	// UpsertPatient creates or updates the Patient resource identified by
	// identifierSystem|identifierValue (our internal user id) and returns
	// its FHIR resource id.
	UpsertPatient(ctx context.Context, p Patient) (id string, err error)

	// UpsertPractitioner creates or updates the Practitioner resource
	// identified by the doctor's SLMC number and returns its FHIR resource
	// id.
	UpsertPractitioner(ctx context.Context, p Practitioner) (id string, err error)

	// UpsertEncounter creates or updates the Encounter resource identified
	// by our internal appointment id and returns its FHIR resource id.
	UpsertEncounter(ctx context.Context, e Encounter) (id string, err error)

	// CreateMedicationRequest creates a new MedicationRequest and returns
	// its FHIR resource id. Prescriptions are immutable once issued, so
	// this is always a create, never an upsert.
	CreateMedicationRequest(ctx context.Context, mr MedicationRequest) (id string, err error)

	// CreateDocumentReference creates a new DocumentReference and returns
	// its FHIR resource id.
	CreateDocumentReference(ctx context.Context, dr DocumentReference) (id string, err error)

	// CreateComposition creates a new Composition -- one finalised or
	// amended clinical note -- and returns its FHIR resource id.
	//
	// This is a create, never an upsert, for the same reason
	// CreateMedicationRequest is: each revision of a signed note is its own
	// immutable document, and an amendment points at the one it replaces via
	// Composition.relatesTo rather than overwriting it. Overwriting would
	// destroy in FHIR exactly what clinical_note_revisions exists to
	// preserve in Postgres.
	CreateComposition(ctx context.Context, c Composition) (id string, err error)

	// CreateCondition creates a new Condition carrying one ICD-10 coded
	// diagnosis, and returns its FHIR resource id.
	CreateCondition(ctx context.Context, c Condition) (id string, err error)
}

package fhir

import (
	"context"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// NoOpClient is the default FHIR binding. It performs no network I/O and
// returns a locally generated, obviously-fake id for every resource, so
// downstream code that persists a *_fhir_reference_id column stores
// something distinguishable from a real Medplum id rather than an empty
// string that would silently pass validation checks.
//
// This is what "nothing breaks when no FHIR server runs" means concretely:
// every other feature (upload, prescribe, verify) works identically whether
// FHIR_BASE_URL is configured or not, and the gap is visible in the stored
// id's "noop:" prefix rather than hidden.
type NoOpClient struct {
	log zerolog.Logger
}

var _ Client = (*NoOpClient)(nil)

// NewNoOp returns a NoOpClient that logs once per call at debug level, so an
// operator who greps logs for "fhir" can tell FHIR integration is disabled
// without that fact being silent.
func NewNoOp(log zerolog.Logger) *NoOpClient {
	return &NoOpClient{log: log.With().Str("component", "fhir_noop").Logger()}
}

func (n *NoOpClient) fakeID(resourceType string) string {
	id := "noop:" + resourceType + ":" + uuid.New().String()
	n.log.Debug().Str("resource_type", resourceType).Msg("fhir integration disabled, using no-op reference")
	return id
}

func (n *NoOpClient) UpsertPatient(context.Context, Patient) (string, error) {
	return n.fakeID("Patient"), nil
}

func (n *NoOpClient) UpsertPractitioner(context.Context, Practitioner) (string, error) {
	return n.fakeID("Practitioner"), nil
}

func (n *NoOpClient) UpsertEncounter(context.Context, Encounter) (string, error) {
	return n.fakeID("Encounter"), nil
}

func (n *NoOpClient) CreateMedicationRequest(context.Context, MedicationRequest) (string, error) {
	return n.fakeID("MedicationRequest"), nil
}

func (n *NoOpClient) CreateDocumentReference(context.Context, DocumentReference) (string, error) {
	return n.fakeID("DocumentReference"), nil
}

func (n *NoOpClient) CreateComposition(context.Context, Composition) (string, error) {
	return n.fakeID("Composition"), nil
}

func (n *NoOpClient) CreateCondition(context.Context, Condition) (string, error) {
	return n.fakeID("Condition"), nil
}

package doctor

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

type recordingActivator struct {
	ids []uuid.UUID
}

func (r *recordingActivator) ActivateApprovedApplication(_ context.Context, id uuid.UUID) error {
	r.ids = append(r.ids, id)
	return nil
}

func TestActivateApprovedAccount_CallsActivator(t *testing.T) {
	rec := &recordingActivator{}
	s := &Service{accounts: rec, log: zerolog.Nop()}
	id := uuid.New()
	s.activateApprovedAccount(context.Background(), id)
	if len(rec.ids) != 1 || rec.ids[0] != id {
		t.Fatalf("activator calls = %v, want [%s]", rec.ids, id)
	}
}

func TestActivateApprovedAccount_NilActivatorIsNoop(t *testing.T) {
	s := &Service{log: zerolog.Nop()}
	s.activateApprovedAccount(context.Background(), uuid.New())
}

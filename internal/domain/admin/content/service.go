package content

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Service wraps the repository with audit staging and outbox publication.
// Specialties/symptoms/drugs changes are published as content.* events so
// doctor-service, scheduling-service and record-service can keep a local
// read copy without a cross-database join (ADR-004); articles are
// admin-console-only and are not published anywhere.
type Service struct {
	pool   database.Pool
	repo   *Repository
	outbox *events.Outbox
}

func NewService(pool database.Pool, repo *Repository, outbox *events.Outbox) *Service {
	return &Service{pool: pool, repo: repo, outbox: outbox}
}

func (s *Service) ListSpecialties(ctx context.Context) ([]Specialty, error) {
	return s.repo.ListSpecialties(ctx)
}

func (s *Service) SaveSpecialty(ctx context.Context, id uuid.UUID, sp Specialty, version int) (Specialty, error) {
	var result Specialty
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		if id == uuid.Nil {
			result, err = s.repo.CreateSpecialty(ctx, sp)
		} else {
			result, err = s.repo.UpdateSpecialty(ctx, id, sp, version)
		}
		if err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectContentSpecialtyUpdated, result.ID.String(), result)
	})
	if err != nil {
		return Specialty{}, err
	}
	audit.Stage(ctx, audit.Draft{Action: "content.specialty_saved", ResourceType: "specialty", ResourceID: result.ID.String(), NewValue: result})
	return result, nil
}

func (s *Service) ListSymptoms(ctx context.Context) ([]Symptom, error) {
	return s.repo.ListSymptoms(ctx)
}

func (s *Service) SaveSymptom(ctx context.Context, id uuid.UUID, sy Symptom, version int) (Symptom, error) {
	var result Symptom
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		if id == uuid.Nil {
			result, err = s.repo.CreateSymptom(ctx, sy)
		} else {
			result, err = s.repo.UpdateSymptom(ctx, id, sy, version)
		}
		if err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectContentSymptomUpdated, result.ID.String(), result)
	})
	if err != nil {
		return Symptom{}, err
	}
	audit.Stage(ctx, audit.Draft{Action: "content.symptom_saved", ResourceType: "symptom", ResourceID: result.ID.String(), NewValue: result})
	return result, nil
}

func (s *Service) ListDrugs(ctx context.Context, search string) ([]Drug, error) {
	return s.repo.ListDrugs(ctx, search)
}

func (s *Service) SaveDrug(ctx context.Context, id uuid.UUID, d Drug, version int) (Drug, error) {
	var result Drug
	err := database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		if id == uuid.Nil {
			result, err = s.repo.CreateDrug(ctx, d)
		} else {
			result, err = s.repo.UpdateDrug(ctx, id, d, version)
		}
		if err != nil {
			return err
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectContentDrugUpdated, result.ID.String(), result)
	})
	if err != nil {
		return Drug{}, err
	}
	audit.Stage(ctx, audit.Draft{Action: "content.drug_saved", ResourceType: "drug", ResourceID: result.ID.String(), NewValue: result})
	return result, nil
}

func (s *Service) ListArticles(ctx context.Context, publishedOnly bool) ([]Article, error) {
	return s.repo.ListArticles(ctx, publishedOnly)
}

func (s *Service) SaveArticle(ctx context.Context, id uuid.UUID, a Article, version int, authorID uuid.UUID) (Article, error) {
	var result Article
	var err error
	if id == uuid.Nil {
		result, err = s.repo.CreateArticle(ctx, a, authorID)
	} else {
		result, err = s.repo.UpdateArticle(ctx, id, a, version)
	}
	if err != nil {
		return Article{}, err
	}
	audit.Stage(ctx, audit.Draft{Action: "content.article_saved", ResourceType: "article", ResourceID: result.ID.String(), NewValue: map[string]any{"title": result.Title, "published": result.Published}})
	return result, nil
}

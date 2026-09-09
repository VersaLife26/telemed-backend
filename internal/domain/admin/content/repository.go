package content

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

var ErrNotFound = errors.New("content: not found")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// --- specialties -------------------------------------------------------

func (r *Repository) ListSpecialties(ctx context.Context) ([]Specialty, error) {
	const q = `SELECT id, code, name_en, name_si, name_ta, active, created_at, updated_at, version
	           FROM specialties WHERE deleted_at IS NULL ORDER BY name_en`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("content: list specialties: %w", err)
	}
	defer rows.Close()
	var out []Specialty
	for rows.Next() {
		var s Specialty
		if err := rows.Scan(&s.ID, &s.Code, &s.NameEN, &s.NameSI, &s.NameTA, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.Version); err != nil {
			return nil, fmt.Errorf("content: scan specialty: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) CreateSpecialty(ctx context.Context, s Specialty) (Specialty, error) {
	const q = `
		INSERT INTO specialties (code, name_en, name_si, name_ta, active)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, code, name_en, name_si, name_ta, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, s.Code, s.NameEN, s.NameSI, s.NameTA, s.Active)
	return scanSpecialty(row)
}

func (r *Repository) UpdateSpecialty(ctx context.Context, id uuid.UUID, s Specialty, version int) (Specialty, error) {
	const q = `
		UPDATE specialties SET name_en=$3, name_si=$4, name_ta=$5, active=$6, updated_at=NOW(), version=version+1
		WHERE id=$1 AND version=$2 AND deleted_at IS NULL
		RETURNING id, code, name_en, name_si, name_ta, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, s.NameEN, s.NameSI, s.NameTA, s.Active)
	sp, err := scanSpecialty(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Specialty{}, ErrNotFound
	}
	return sp, err
}

func scanSpecialty(row rowScanner) (Specialty, error) {
	var s Specialty
	err := row.Scan(&s.ID, &s.Code, &s.NameEN, &s.NameSI, &s.NameTA, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.Version)
	if err != nil {
		return Specialty{}, fmt.Errorf("content: scan specialty: %w", err)
	}
	return s, nil
}

// --- symptoms ------------------------------------------------------------

func (r *Repository) ListSymptoms(ctx context.Context) ([]Symptom, error) {
	const q = `SELECT id, code, name_en, name_si, name_ta, specialty_codes, active, created_at, updated_at, version
	           FROM symptoms WHERE deleted_at IS NULL ORDER BY name_en`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("content: list symptoms: %w", err)
	}
	defer rows.Close()
	var out []Symptom
	for rows.Next() {
		s, err := scanSymptom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) CreateSymptom(ctx context.Context, s Symptom) (Symptom, error) {
	const q = `
		INSERT INTO symptoms (code, name_en, name_si, name_ta, specialty_codes, active)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, code, name_en, name_si, name_ta, specialty_codes, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, s.Code, s.NameEN, s.NameSI, s.NameTA, s.SpecialtyCodes, s.Active)
	return scanSymptom(row)
}

func (r *Repository) UpdateSymptom(ctx context.Context, id uuid.UUID, s Symptom, version int) (Symptom, error) {
	const q = `
		UPDATE symptoms SET name_en=$3, name_si=$4, name_ta=$5, specialty_codes=$6, active=$7,
		       updated_at=NOW(), version=version+1
		WHERE id=$1 AND version=$2 AND deleted_at IS NULL
		RETURNING id, code, name_en, name_si, name_ta, specialty_codes, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, s.NameEN, s.NameSI, s.NameTA, s.SpecialtyCodes, s.Active)
	sy, err := scanSymptom(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Symptom{}, ErrNotFound
	}
	return sy, err
}

func scanSymptom(row rowScanner) (Symptom, error) {
	var s Symptom
	err := row.Scan(&s.ID, &s.Code, &s.NameEN, &s.NameSI, &s.NameTA, &s.SpecialtyCodes, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.Version)
	if err != nil {
		return Symptom{}, fmt.Errorf("content: scan symptom: %w", err)
	}
	return s, nil
}

// --- drugs -----------------------------------------------------------------

func (r *Repository) ListDrugs(ctx context.Context, search string) ([]Drug, error) {
	q := `SELECT id, name, strength, form, manufacturer, active, created_at, updated_at, version
	      FROM drugs WHERE deleted_at IS NULL`
	args := []any{}
	if search != "" {
		args = append(args, "%"+search+"%")
		q += " AND name ILIKE $1"
	}
	q += " ORDER BY name"

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("content: list drugs: %w", err)
	}
	defer rows.Close()
	var out []Drug
	for rows.Next() {
		d, err := scanDrug(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repository) CreateDrug(ctx context.Context, d Drug) (Drug, error) {
	const q = `
		INSERT INTO drugs (name, strength, form, manufacturer, active)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, name, strength, form, manufacturer, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, d.Name, d.Strength, d.Form, d.Manufacturer, d.Active)
	return scanDrug(row)
}

func (r *Repository) UpdateDrug(ctx context.Context, id uuid.UUID, d Drug, version int) (Drug, error) {
	const q = `
		UPDATE drugs SET name=$3, strength=$4, form=$5, manufacturer=$6, active=$7, updated_at=NOW(), version=version+1
		WHERE id=$1 AND version=$2 AND deleted_at IS NULL
		RETURNING id, name, strength, form, manufacturer, active, created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, d.Name, d.Strength, d.Form, d.Manufacturer, d.Active)
	dr, err := scanDrug(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Drug{}, ErrNotFound
	}
	return dr, err
}

func scanDrug(row rowScanner) (Drug, error) {
	var d Drug
	err := row.Scan(&d.ID, &d.Name, &d.Strength, &d.Form, &d.Manufacturer, &d.Active, &d.CreatedAt, &d.UpdatedAt, &d.Version)
	if err != nil {
		return Drug{}, fmt.Errorf("content: scan drug: %w", err)
	}
	return d, nil
}

// --- articles ----------------------------------------------------------

func (r *Repository) ListArticles(ctx context.Context, publishedOnly bool) ([]Article, error) {
	q := `SELECT id, title, slug, body, language, specialty_code, published, published_at, author_admin_id,
	             created_at, updated_at, version
	      FROM articles WHERE deleted_at IS NULL`
	if publishedOnly {
		q += " AND published = TRUE"
	}
	q += " ORDER BY created_at DESC"

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("content: list articles: %w", err)
	}
	defer rows.Close()
	var out []Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) CreateArticle(ctx context.Context, a Article, authorID uuid.UUID) (Article, error) {
	const q = `
		INSERT INTO articles (title, slug, body, language, specialty_code, published, published_at, author_admin_id)
		VALUES ($1,$2,$3,$4,$5,$6, CASE WHEN $6 THEN NOW() ELSE NULL END, $7)
		RETURNING id, title, slug, body, language, specialty_code, published, published_at, author_admin_id,
		          created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, a.Title, a.Slug, a.Body, a.Language, a.SpecialtyCode, a.Published, authorID)
	return scanArticle(row)
}

func (r *Repository) UpdateArticle(ctx context.Context, id uuid.UUID, a Article, version int) (Article, error) {
	const q = `
		UPDATE articles SET title=$3, body=$4, language=$5, specialty_code=$6, published=$7,
		       published_at = CASE WHEN $7 AND published_at IS NULL THEN NOW() WHEN NOT $7 THEN NULL ELSE published_at END,
		       updated_at=NOW(), version=version+1
		WHERE id=$1 AND version=$2 AND deleted_at IS NULL
		RETURNING id, title, slug, body, language, specialty_code, published, published_at, author_admin_id,
		          created_at, updated_at, version`
	row := r.pool.QueryRow(ctx, q, id, version, a.Title, a.Body, a.Language, a.SpecialtyCode, a.Published)
	art, err := scanArticle(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Article{}, ErrNotFound
	}
	return art, err
}

func scanArticle(row rowScanner) (Article, error) {
	var a Article
	err := row.Scan(&a.ID, &a.Title, &a.Slug, &a.Body, &a.Language, &a.SpecialtyCode, &a.Published,
		&a.PublishedAt, &a.AuthorAdminID, &a.CreatedAt, &a.UpdatedAt, &a.Version)
	if err != nil {
		return Article{}, fmt.Errorf("content: scan article: %w", err)
	}
	return a, nil
}

type rowScanner interface{ Scan(dest ...any) error }

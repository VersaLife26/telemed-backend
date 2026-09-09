package doctor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SpecialtyExists checks a specialty code against the seeded reference
// table. The doctors.specialty column also carries a foreign key to
// specialties(code), so this is a pre-check for a clean 422 rather than the
// only line of defence -- a race between this check and the insert still
// ends in a database-level rejection, not a bad row.
func (r *Repository) SpecialtyExists(ctx context.Context, code string) (bool, error) {
	const q = `SELECT 1 FROM specialties WHERE code = $1 AND is_active = TRUE`
	var one int
	err := r.pool.QueryRow(ctx, q, code).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("doctor: check specialty: %w", err)
	}
	return true, nil
}

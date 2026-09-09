package doctor

import (
	"strings"
	"testing"
	"time"
)

// TestBuildSearchQueryFilterCombinations exercises the pure query builder
// against every filter in isolation and a handful of realistic combinations,
// asserting the right SQL fragments and bound parameters appear -- and, just
// as importantly, that filters which were not requested do not leak into the
// WHERE clause or the parameter list.
func TestBuildSearchQueryFilterCombinations(t *testing.T) {
	minFee := int64(100000)
	maxFee := int64(500000)
	minRating := 4.0

	tests := []struct {
		name          string
		filters       SearchFilters
		wantContains  []string
		wantArgs      []any
		wantJoin      bool
		skipArgsCheck bool
	}{
		{
			name:         "no filters still scopes to approved and not deleted",
			filters:      SearchFilters{Page: 1, PerPage: 20},
			wantContains: []string{"d.verification_status = 'approved'", "d.deleted_at IS NULL"},
			wantArgs:     nil,
			wantJoin:     false,
		},
		{
			name:         "specialty only",
			filters:      SearchFilters{Specialty: "cardiology", Page: 1, PerPage: 20},
			wantContains: []string{"d.specialty = $1"},
			wantArgs:     []any{"cardiology"},
			wantJoin:     false,
		},
		{
			name:         "language only",
			filters:      SearchFilters{Language: "si", Page: 1, PerPage: 20},
			wantContains: []string{"$1 = ANY(d.languages)"},
			wantArgs:     []any{"si"},
			wantJoin:     false,
		},
		{
			name:         "fee range",
			filters:      SearchFilters{MinFeeCents: &minFee, MaxFeeCents: &maxFee, Page: 1, PerPage: 20},
			wantContains: []string{"d.fee_cents >= $1", "d.fee_cents <= $2"},
			wantArgs:     []any{minFee, maxFee},
			wantJoin:     false,
		},
		{
			name:         "min rating only",
			filters:      SearchFilters{MinRating: &minRating, Page: 1, PerPage: 20},
			wantContains: []string{"d.rating >= $1"},
			wantArgs:     []any{minRating},
			wantJoin:     false,
		},
		{
			name:         "free text query",
			filters:      SearchFilters{Query: "cardiologist colombo", Page: 1, PerPage: 20},
			wantContains: []string{"d.search_vector @@ websearch_to_tsquery('english', $1)"},
			wantArgs:     []any{"cardiologist colombo"},
			wantJoin:     false,
		},
		{
			name:         "available now requires the join and the not-null filter",
			filters:      SearchFilters{RequireAvailable: true, AvailableAfter: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), Page: 1, PerPage: 20},
			wantContains: []string{"LEFT JOIN LATERAL", "das.next_available_at >= $1", "avail.next_available_at IS NOT NULL"},
			wantArgs:     []any{time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)},
			wantJoin:     true,
		},
		{
			// AvailableAfter is left zero here on purpose: buildSearchQuery
			// must default it to "now" so the join always has a bound
			// parameter, even when the caller only wants the join for
			// sorting and never set RequireAvailable. The exact default
			// timestamp is time.Now()-dependent, so this case only checks
			// shape, not the bound value -- see skipArgsCheck below.
			name:          "sort by next_available requires the join but not the not-null filter",
			filters:       SearchFilters{SortBy: SortNextAvailable, Page: 1, PerPage: 20},
			wantContains:  []string{"LEFT JOIN LATERAL", "das.next_available_at >= $1"},
			wantJoin:      true,
			skipArgsCheck: true,
		},
		{
			name: "kitchen sink: every filter together, in declaration order",
			filters: SearchFilters{
				Specialty: "cardiology", Language: "en", MinFeeCents: &minFee, MaxFeeCents: &maxFee,
				MinRating: &minRating, Query: "senior", RequireAvailable: true,
				AvailableAfter: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), SortBy: SortRating, Page: 2, PerPage: 10,
			},
			wantContains: []string{
				"d.specialty = $1", "$2 = ANY(d.languages)", "d.fee_cents >= $3", "d.fee_cents <= $4",
				"d.rating >= $5", "websearch_to_tsquery('english', $6)", "das.next_available_at >= $7",
				"avail.next_available_at IS NOT NULL",
			},
			wantArgs: []any{"cardiology", "en", minFee, maxFee, minRating, "senior", time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)},
			wantJoin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sq := buildSearchQuery(tt.filters)

			for _, want := range tt.wantContains {
				if !strings.Contains(sq.whereClause+sq.joinClause, want) {
					t.Errorf("expected query to contain %q\nwhere: %s\njoin: %s", want, sq.whereClause, sq.joinClause)
				}
			}
			if sq.needsJoin != tt.wantJoin {
				t.Errorf("needsJoin = %v, want %v", sq.needsJoin, tt.wantJoin)
			}
			if tt.skipArgsCheck {
				return
			}
			if len(sq.args) != len(tt.wantArgs) {
				t.Fatalf("args = %#v (len %d), want %#v (len %d)", sq.args, len(sq.args), tt.wantArgs, len(tt.wantArgs))
			}
			for i, want := range tt.wantArgs {
				if sq.args[i] != want {
					t.Errorf("args[%d] = %#v, want %#v", i, sq.args[i], want)
				}
			}
		})
	}
}

// TestBuildSearchQueryNeverLeaksUnrequestedAvailabilityJoin guards the
// performance-sensitive default: a plain specialty/fee search must not pay
// for the availability LATERAL join it never asked for.
func TestBuildSearchQueryNeverLeaksUnrequestedAvailabilityJoin(t *testing.T) {
	sq := buildSearchQuery(SearchFilters{Specialty: "cardiology", SortBy: SortRating, Page: 1, PerPage: 20})
	if sq.needsJoin {
		t.Fatal("expected no availability join for a plain rating-sorted search")
	}
	if strings.Contains(sq.joinClause, "LATERAL") {
		t.Fatal("join clause should be empty when availability was not requested")
	}
}

// TestOrderByForKnownSorts locks down the ORDER BY each sort option produces,
// including the default (empty string / unknown falls back to rating).
func TestOrderByForKnownSorts(t *testing.T) {
	tests := map[SortField]string{
		SortRating:        "d.rating DESC, d.review_count DESC, d.id ASC",
		SortFee:           "d.fee_cents ASC, d.id ASC",
		SortExperience:    "d.experience_years DESC, d.id ASC",
		SortNextAvailable: "avail.next_available_at ASC NULLS LAST, d.id ASC",
		"":                "d.rating DESC, d.review_count DESC, d.id ASC",
		"garbage":         "d.rating DESC, d.review_count DESC, d.id ASC",
	}
	for sort, want := range tests {
		if got := orderByFor(sort); got != want {
			t.Errorf("orderByFor(%q) = %q, want %q", sort, got, want)
		}
	}
}

// TestSortFieldValid covers the whitelist the search handler validates
// against before any filter is even built.
func TestSortFieldValid(t *testing.T) {
	valid := []SortField{SortRating, SortFee, SortExperience, SortNextAvailable, ""}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if SortField("bogus").Valid() {
		t.Error(`"bogus" should not be valid`)
	}
}

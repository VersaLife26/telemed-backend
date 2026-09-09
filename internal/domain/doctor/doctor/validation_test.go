package doctor

import (
	"testing"

	"telemed/internal/platform/httpx"
)

// TestSLMCNumberValidation is table-driven over the SLMC registration number
// format (httpx's "slmc" validator tag, ^[A-Z]{0,3}[0-9]{4,8}$ applied
// case-insensitively): historic bare-digit numbers and newer letter-prefixed
// ones must both pass; anything with an internal separator, wrong digit
// count, or garbage must not.
func TestSLMCNumberValidation(t *testing.T) {
	type req struct {
		SLMCNumber string `validate:"required,slmc"`
	}

	tests := []struct {
		name    string
		number  string
		wantErr bool
	}{
		{"bare four digits, historic minimum", "1234", false},
		{"bare eight digits, historic maximum", "12345678", false},
		{"single letter prefix", "A12345", false},
		{"two letter prefix", "SL1234", false},
		{"three letter prefix", "SLM1234", false},
		{"lowercase prefix normalises", "slm1234", false},
		{"empty", "", true},
		{"too few digits", "123", true},
		{"too many digits", "123456789", true},
		{"SLMC prefix accepted -- doctors and our own seed data both type it", "SLMC1234", false},
		{"an arbitrary four-letter prefix is still rejected", "XYZQ1234", true},
		{"contains a hyphen", "SL-1234", true},
		{"contains a space", "SL 1234", true},
		{"letters after digits", "1234SL", true},
		{"only letters, no digits", "SLMC", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.Validator().Struct(req{SLMCNumber: tt.number})
			if tt.wantErr && err == nil {
				t.Errorf("SLMC %q: expected validation error, got none", tt.number)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("SLMC %q: unexpected validation error: %v", tt.number, err)
			}
		})
	}
}

// TestValidateNoOverlap covers the working-hours overlap rule the database's
// UNIQUE(doctor_id, day_of_week, start_time) constraint cannot catch on its
// own: two windows on the same day with different start times can still
// overlap (09:00-11:00 and 10:00-12:00).
func TestValidateNoOverlap(t *testing.T) {
	tests := []struct {
		name    string
		hours   []WorkingHour
		wantErr bool
	}{
		{
			name:  "empty schedule",
			hours: nil,
		},
		{
			name: "single window",
			hours: []WorkingHour{
				{DayOfWeek: 1, StartTime: "09:00:00", EndTime: "17:00:00"},
			},
		},
		{
			name: "back to back, no gap, does not overlap",
			hours: []WorkingHour{
				{DayOfWeek: 1, StartTime: "09:00:00", EndTime: "12:00:00"},
				{DayOfWeek: 1, StartTime: "12:00:00", EndTime: "17:00:00"},
			},
		},
		{
			name: "same start time different days is fine",
			hours: []WorkingHour{
				{DayOfWeek: 1, StartTime: "09:00:00", EndTime: "12:00:00"},
				{DayOfWeek: 2, StartTime: "09:00:00", EndTime: "12:00:00"},
			},
		},
		{
			name: "overlapping windows same day",
			hours: []WorkingHour{
				{DayOfWeek: 1, StartTime: "09:00:00", EndTime: "11:00:00"},
				{DayOfWeek: 1, StartTime: "10:00:00", EndTime: "12:00:00"},
			},
			wantErr: true,
		},
		{
			name: "identical windows same day",
			hours: []WorkingHour{
				{DayOfWeek: 3, StartTime: "09:00:00", EndTime: "17:00:00"},
				{DayOfWeek: 3, StartTime: "09:00:00", EndTime: "17:00:00"},
			},
			wantErr: true,
		},
		{
			name: "one window fully contains another",
			hours: []WorkingHour{
				{DayOfWeek: 5, StartTime: "08:00:00", EndTime: "18:00:00"},
				{DayOfWeek: 5, StartTime: "12:00:00", EndTime: "13:00:00"},
			},
			wantErr: true,
		},
		{
			name: "three windows, only the last two overlap",
			hours: []WorkingHour{
				{DayOfWeek: 1, StartTime: "06:00:00", EndTime: "08:00:00"},
				{DayOfWeek: 1, StartTime: "09:00:00", EndTime: "11:00:00"},
				{DayOfWeek: 1, StartTime: "10:30:00", EndTime: "12:00:00"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNoOverlap(tt.hours)
			if tt.wantErr && err == nil {
				t.Error("expected ErrOverlappingHours, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

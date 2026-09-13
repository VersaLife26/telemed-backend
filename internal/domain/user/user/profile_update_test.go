package user

import (
	"testing"
	"time"
)

func TestApplyProfilePatch_OverlaysOnlyProvidedFields(t *testing.T) {
	email := "keep@example.com"
	phone := "+94771234567"
	dob := time.Date(1990, 6, 15, 0, 0, 0, 0, time.UTC)
	u := User{
		Name: "Old", Phone: phone, Email: &email, Address: "12 Galle Rd",
		DateOfBirth: &dob, Language: LanguageEnglish, Version: 3,
	}

	newEmail := "new@example.com"
	ApplyProfilePatch(&u, UpdateProfileInput{
		Name: "New Name", Email: &newEmail, Language: LanguageSinhala, Version: 3,
	})

	if u.Name != "New Name" {
		t.Errorf("name = %q, want New Name", u.Name)
	}
	if u.Phone != phone {
		t.Errorf("omitted phone changed to %q", u.Phone)
	}
	if u.Email == nil || *u.Email != newEmail {
		t.Errorf("email = %v, want %s", u.Email, newEmail)
	}
	if u.Address != "12 Galle Rd" {
		t.Errorf("omitted address changed to %q", u.Address)
	}
	if u.DateOfBirth == nil || !u.DateOfBirth.Equal(dob) {
		t.Errorf("omitted date of birth changed")
	}
	if u.Language != LanguageSinhala {
		t.Errorf("language = %s, want si", u.Language)
	}
}

func TestApplyProfilePatch_ClearsDateOfBirth(t *testing.T) {
	dob := time.Date(1988, 1, 2, 0, 0, 0, 0, time.UTC)
	u := User{Name: "Pat", DateOfBirth: &dob}
	ApplyProfilePatch(&u, UpdateProfileInput{Name: "Pat", ClearDOB: true, Language: LanguageEnglish})
	if u.DateOfBirth != nil {
		t.Errorf("date of birth still set: %v", u.DateOfBirth)
	}
}

func TestHasLoginIdentity(t *testing.T) {
	email := "a@b.co"
	sub := "google-sub"
	tests := []struct {
		name string
		u    User
		want bool
	}{
		{"phone only", User{Phone: "+94771234567"}, true},
		{"email only", User{Email: &email}, true},
		{"google only", User{GoogleSub: &sub}, true},
		{"empty", User{}, false},
		{"blank email pointer", User{Email: new(string)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasLoginIdentity(&tt.u); got != tt.want {
				t.Errorf("hasLoginIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}

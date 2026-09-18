package finance

import (
	"testing"
)

func TestDecodeCommissionValue_editorShape(t *testing.T) {
	rule, err := DecodeCommissionValue([]byte(`{
		"default_commission_percent": 18,
		"rules": [{"specialty": "GP", "commission_percent": 20}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if rule.DefaultPercent != 18 {
		t.Fatalf("default = %d, want 18", rule.DefaultPercent)
	}
	if rule.BySpecialty["GP"] != 20 {
		t.Fatalf("GP = %v", rule.BySpecialty)
	}
}

func TestDecodeCommissionValue_wireShape(t *testing.T) {
	rule, err := DecodeCommissionValue([]byte(`{"default_percent": 15, "by_specialty": {"cardio": 12}}`))
	if err != nil {
		t.Fatal(err)
	}
	if rule.DefaultPercent != 15 || rule.BySpecialty["cardio"] != 12 {
		t.Fatalf("got %+v", rule)
	}
}

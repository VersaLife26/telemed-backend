package scheduling

import (
	"testing"
	"time"
)

func TestAgeAtVisit(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	dob := time.Date(1990, 6, 15, 0, 0, 0, 0, loc)
	visit := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	if got := AgeAtVisit(dob, visit, loc); got != 35 {
		t.Fatalf("day before birthday: got %d want 35", got)
	}
	visit2 := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	if got := AgeAtVisit(dob, visit2, loc); got != 36 {
		t.Fatalf("on birthday: got %d want 36", got)
	}
}

func TestParseVisitDOB(t *testing.T) {
	d, err := ParseVisitDOB("2010-05-01")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Format("2006-01-02") != "2010-05-01" {
		t.Fatalf("got %s", d.Format("2006-01-02"))
	}
	if _, err := ParseVisitDOB("not-a-date"); err == nil {
		t.Fatal("expected error for bad dob")
	}
}

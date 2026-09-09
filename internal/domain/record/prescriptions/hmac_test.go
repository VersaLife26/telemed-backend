package prescriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func samplePrescription() Prescription {
	return Prescription{
		ID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		DoctorID:  uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		PatientID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		IssuedAt:  time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC),
		Items: []Item{
			{DrugName: "Amoxicillin", Strength: "500mg", Form: "capsule", Dosage: "1 capsule", Frequency: "3x daily", DurationDays: 7, Quantity: 21, SortOrder: 0},
			{DrugName: "Paracetamol", Strength: "500mg", Form: "tablet", Dosage: "1 tablet", Frequency: "as needed", DurationDays: 5, Quantity: 10, SortOrder: 1},
		},
	}
}

func TestSignAndVerify_Valid(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	p.VerificationHMAC = Sign(secret, p)

	if !Verify(secret, p, p.VerificationHMAC) {
		t.Fatal("expected a freshly signed prescription to verify")
	}
}

func TestVerify_TamperedItemContent(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	// A pharmacist would trust this dosage; an attacker (or a bug) changes
	// it after issuance without re-signing.
	tampered := p
	tampered.Items = append([]Item{}, p.Items...)
	tampered.Items[0].Dosage = "2 capsules"

	if Verify(secret, tampered, h) {
		t.Fatal("changing a dosage after signing must invalidate the HMAC")
	}
}

func TestVerify_TamperedItemQuantity(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	tampered := p
	tampered.Items = append([]Item{}, p.Items...)
	tampered.Items[1].Quantity = 100 // a pharmacist dispensing 100 instead of 10

	if Verify(secret, tampered, h) {
		t.Fatal("changing a quantity after signing must invalidate the HMAC")
	}
}

func TestVerify_TamperedID(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	tampered := p
	tampered.ID = uuid.MustParse("99999999-9999-9999-9999-999999999999")

	if Verify(secret, tampered, h) {
		t.Fatal("changing the id after signing must invalidate the HMAC")
	}
}

func TestVerify_TamperedDoctorOrPatient(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	byDoctor := p
	byDoctor.DoctorID = uuid.New()
	if Verify(secret, byDoctor, h) {
		t.Fatal("changing doctor_id after signing must invalidate the HMAC")
	}

	byPatient := p
	byPatient.PatientID = uuid.New()
	if Verify(secret, byPatient, h) {
		t.Fatal("changing patient_id after signing must invalidate the HMAC")
	}
}

func TestVerify_TamperedIssuedAt(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	tampered := p
	tampered.IssuedAt = p.IssuedAt.Add(48 * time.Hour) // backdating/postdating a prescription

	if Verify(secret, tampered, h) {
		t.Fatal("changing issued_at after signing must invalidate the HMAC")
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	p := samplePrescription()
	h := Sign([]byte("secret-a-long-enough-to-pass-any-length-checks"), p)

	if Verify([]byte("secret-b-a-completely-different-value-here!!"), p, h) {
		t.Fatal("verifying with the wrong secret must fail")
	}
}

func TestVerify_MalformedHex(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()

	for _, bad := range []string{"", "not-hex-at-all", "zz", "12345"} {
		if Verify(secret, p, bad) {
			t.Fatalf("malformed hmac %q must not verify", bad)
		}
	}
}

func TestVerify_ExtraOrMissingItem(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()
	h := Sign(secret, p)

	// A pharmacist should be able to detect a smuggled-in extra drug line.
	withExtra := p
	withExtra.Items = append(append([]Item{}, p.Items...), Item{
		DrugName: "Tramadol", Strength: "50mg", Form: "capsule", Dosage: "1 capsule",
		Frequency: "2x daily", DurationDays: 5, Quantity: 10, SortOrder: 2,
	})
	if Verify(secret, withExtra, h) {
		t.Fatal("an added item must invalidate the HMAC")
	}

	withMissing := p
	withMissing.Items = p.Items[:1]
	if Verify(secret, withMissing, h) {
		t.Fatal("a removed item must invalidate the HMAC")
	}
}

func TestItemsDigest_OrderIndependentOfSliceConstruction(t *testing.T) {
	items := []Item{
		{DrugName: "A", Dosage: "1", Frequency: "1x", DurationDays: 1, Quantity: 1, SortOrder: 0},
		{DrugName: "B", Dosage: "1", Frequency: "1x", DurationDays: 1, Quantity: 1, SortOrder: 1},
	}
	reversed := []Item{items[1], items[0]} // same logical set, built in a different slice order

	if ItemsDigest(items) != ItemsDigest(reversed) {
		t.Fatal("ItemsDigest must be independent of input slice order, only SortOrder")
	}
}

func TestItemsDigest_DelimiterInjectionDoesNotCollide(t *testing.T) {
	// Two different item sets that could hash identically if pipe-delimited
	// fields were naively concatenated without per-item boundaries.
	a := []Item{{DrugName: "X|Y", Dosage: "1", Frequency: "1x", DurationDays: 1, Quantity: 1}}
	b := []Item{{DrugName: "X", Dosage: "Y|1", Frequency: "1x", DurationDays: 1, Quantity: 1}}

	if ItemsDigest(a) == ItemsDigest(b) {
		t.Fatal("distinct item sets must not produce the same digest via delimiter confusion")
	}
}

func TestSign_Deterministic(t *testing.T) {
	secret := []byte("a-very-secret-key-that-is-long-enough")
	p := samplePrescription()

	first := Sign(secret, p)
	second := Sign(secret, p)
	if first != second {
		t.Fatalf("signing the same prescription twice must produce the same value: %q != %q", first, second)
	}
}

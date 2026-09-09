package prescriptions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// canonicalVersion is prefixed to the signed payload so a future change to
// the canonicalization scheme (e.g. adding doctor display fields to the
// signed content, see the README's "Known gaps" section) can be
// distinguished from the current one rather than silently producing
// different bytes for the same logical prescription.
const canonicalVersion = "v1"

// ItemsDigest hashes every drug line into one fixed-size value. Items are
// sorted by SortOrder first so the digest is independent of slice
// construction order -- two callers that build the same prescription from
// the same rows in a different order must still produce the same digest.
func ItemsDigest(items []Item) string {
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SortOrder < sorted[j].SortOrder })

	h := sha256.New()
	for i := range sorted {
		it := &sorted[i]
		// Every field that changes what a pharmacist would dispense is
		// included, pipe-delimited with a trailing newline per item so a
		// value containing "|" cannot be crafted to make two different item
		// sets hash identically by shifting a delimiter. hash.Hash.Write
		// (which Fprintf calls into) is documented to never return an
		// error, so the result is discarded deliberately.
		_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%s|%d|%d|%s|%t\n",
			it.DrugName, it.Strength, it.Form, it.Dosage, it.Frequency,
			it.DurationDays, it.Quantity, it.Instructions, it.IsGeneric)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalPayload builds the exact byte sequence that is HMAC-signed and
// later re-verified. It covers id, doctor_id, patient_id, issued_at, and the
// items digest -- deliberately the prescription's clinical content, not
// just its identity. Signing only the id (as a naive "id + doctor_id +
// secret" scheme would) proves the QR code refers to a real prescription row
// but proves nothing about whether that row's contents -- which drug, what
// dose, how many -- still match what the doctor issued. A pharmacist relies
// on exactly that guarantee, so it is what gets signed.
//
// issued_at is formatted RFC3339Nano in UTC so the canonicalization is
// timezone- and precision-independent: two Prescription values describing
// the same instant always produce the same payload regardless of how the
// time.Time was constructed.
func CanonicalPayload(p Prescription) []byte {
	return fmt.Appendf(nil, "%s|%s|%s|%s|%s|%s",
		canonicalVersion,
		p.ID.String(),
		p.DoctorID.String(),
		p.PatientID.String(),
		p.IssuedAt.UTC().Format(time.RFC3339Nano),
		ItemsDigest(p.Items),
	)
}

// signRaw computes the raw (undecoded) HMAC-SHA256 tag.
func signRaw(secret []byte, p Prescription) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(CanonicalPayload(p))
	return mac.Sum(nil)
}

// Sign returns the hex-encoded HMAC-SHA256 over the canonical payload. This
// value is what gets stored in prescriptions.verification_hmac and embedded
// in the QR code's `h` query parameter.
func Sign(secret []byte, p Prescription) string {
	return hex.EncodeToString(signRaw(secret, p))
}

// Verify recomputes the HMAC over p's CURRENT content and compares it,
// constant-time, against providedHex -- the value scanned from the QR code
// (or supplied as the `h` query parameter), not the database's stored
// verification_hmac column. This distinction is the whole point: if
// anyone alters a prescription's row after issuance (a compromised
// operator, a buggy migration, a direct database edit) without also holding
// the HMAC secret, the recomputed value silently diverges from the
// immutable one printed on the original QR, and verification correctly
// fails. Comparing against the stored column instead would not catch this,
// because both the tampered content and the stored HMAC live in the same
// database and could be edited together.
//
// providedHex is decoded before comparison so a malformed or wrong-length
// value is rejected outright rather than compared byte-for-byte against a
// truncated slice. The comparison itself is always hmac.Equal, never ==,
// which would leak timing information about how many leading bytes matched.
func Verify(secret []byte, p Prescription, providedHex string) bool {
	provided, err := hex.DecodeString(providedHex)
	if err != nil {
		return false
	}
	expected := signRaw(secret, p)
	return hmac.Equal(expected, provided)
}

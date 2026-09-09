package user

import (
	"strconv"
	"testing"
)

// TestGenerateOTP_Format asserts every code is exactly 6 numeric digits,
// zero-padded, and within [0, 1000000) -- the range VerifyOTPHash and the
// SMS body assume.
func TestGenerateOTP_Format(t *testing.T) {
	for range 2000 {
		code, err := GenerateOTP()
		if err != nil {
			t.Fatalf("GenerateOTP: %v", err)
		}
		if len(code) != otpDigits {
			t.Fatalf("code %q has length %d, want %d", code, len(code), otpDigits)
		}
		n, err := strconv.Atoi(code)
		if err != nil {
			t.Fatalf("code %q is not numeric: %v", code, err)
		}
		if n < 0 || n >= 1_000_000 {
			t.Fatalf("code %q out of range", code)
		}
	}
}

// TestGenerateOTP_NoModuloBias is the test AGENT-BRIEF calls for directly:
// "OTP generation distribution (assert no modulo bias)".
//
// The bug this guards against: computing a digit as `randomByte() % 10`
// is biased, because 256 is not a multiple of 10 -- values 0-5 map from 26
// source bytes each (0-5, 26-31, ... wrapping) while 6-9 map from only 25,
// so digits 0-5 are measurably more likely than 6-9 over enough samples.
// GenerateOTP instead draws from crypto/rand.Int(reader, 1_000_000), which
// performs rejection sampling internally and is uniform over the full
// 6-digit space by construction.
//
// A biased generator would fail this test by a wide margin; a correct one
// passes with very high probability by design (a real chi-square variate
// with 9 degrees of freedom exceeds the chosen threshold under 1 in 10^5
// runs), so the threshold is deliberately generous to avoid a flaky CI run
// while still being tight enough to catch real modulo bias.
func TestGenerateOTP_NoModuloBias(t *testing.T) {
	const (
		samples   = 30000 // 30,000 codes * 6 digits = 180,000 digit observations
		buckets   = 10
		threshold = 45.0 // chi-square critical value at 9 dof is ~27.9 for p=0.001
	)

	var counts [buckets]int
	for range samples {
		code, err := GenerateOTP()
		if err != nil {
			t.Fatalf("GenerateOTP: %v", err)
		}
		for _, r := range code {
			counts[r-'0']++
		}
	}

	total := samples * otpDigits
	expected := float64(total) / buckets

	var chiSquare float64
	for digit, c := range counts {
		diff := float64(c) - expected
		chiSquare += (diff * diff) / expected
		t.Logf("digit %d: observed=%d expected=%.1f", digit, c, expected)
	}

	if chiSquare > threshold {
		t.Errorf("chi-square statistic = %.2f, exceeds threshold %.2f -- distribution looks biased (possible modulo bias regression)", chiSquare, threshold)
	}
}

// TestGenerateOTP_Uniqueness is a coarse sanity check: 6-digit codes must
// not collide constantly, which would indicate the RNG is not actually
// being consulted per call (e.g. a seed reused across goroutines).
func TestGenerateOTP_Uniqueness(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	collisions := 0
	for range n {
		code, err := GenerateOTP()
		if err != nil {
			t.Fatalf("GenerateOTP: %v", err)
		}
		if _, dup := seen[code]; dup {
			collisions++
		}
		seen[code] = struct{}{}
	}
	// With a uniform draw from 10^6 values, ~5000 draws yields an expected
	// number of birthday collisions around 12 (5000^2 / (2*10^6)). Anything
	// wildly higher suggests a broken or narrowed random source.
	if collisions > 100 {
		t.Errorf("saw %d collisions in %d draws, far more than expected for a uniform 10^6 space", collisions, n)
	}
}

func TestHashOTP_VerifyRoundtrip(t *testing.T) {
	code, err := GenerateOTP()
	if err != nil {
		t.Fatalf("GenerateOTP: %v", err)
	}
	hash, err := HashOTP(code)
	if err != nil {
		t.Fatalf("HashOTP: %v", err)
	}
	if hash == code {
		t.Fatal("HashOTP returned the code unmodified -- it must never be stored in plaintext")
	}
	if !VerifyOTPHash(hash, code) {
		t.Error("VerifyOTPHash rejected the correct code")
	}
}

func TestVerifyOTPHash_RejectsWrongCode(t *testing.T) {
	hash, err := HashOTP("123456")
	if err != nil {
		t.Fatalf("HashOTP: %v", err)
	}
	tests := []string{"123457", "654321", "000000", "", "12345", "1234567"}
	for _, wrong := range tests {
		if VerifyOTPHash(hash, wrong) {
			t.Errorf("VerifyOTPHash(%q) against hash of 123456 = true, want false", wrong)
		}
	}
}

func TestHashOTP_SaltsDistinctly(t *testing.T) {
	// bcrypt salts every hash independently, so hashing the same code twice
	// must never produce the same ciphertext. This is what stops an operator
	// (or an attacker with Redis read access) from spotting a repeated code
	// across users just by eyeballing the dump.
	h1, err := HashOTP("111111")
	if err != nil {
		t.Fatalf("HashOTP: %v", err)
	}
	h2, err := HashOTP("111111")
	if err != nil {
		t.Fatalf("HashOTP: %v", err)
	}
	if h1 == h2 {
		t.Error("hashing the same code twice produced identical hashes -- bcrypt salt is not varying")
	}
	if !VerifyOTPHash(h1, "111111") || !VerifyOTPHash(h2, "111111") {
		t.Error("both hashes must still verify the original code despite differing salts")
	}
}

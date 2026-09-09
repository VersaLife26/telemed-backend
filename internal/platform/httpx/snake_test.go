package httpx

import "testing"

func TestToSnakeAcronyms(t *testing.T) {
	for in, want := range map[string]string{
		"SLMCNumber": "slmc_number", "DoctorID": "doctor_id", "FeeLKR": "fee_lkr",
		"NICHash": "nic_hash", "StartAt": "start_at", "OTPCode": "otp_code",
		"ID": "id", "Phone": "phone", "AmountCents": "amount_cents",
	} {
		if got := toSnake(in); got != want {
			t.Errorf("toSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

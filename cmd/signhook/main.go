// Command signhook signs a mock-rail webhook body with the real Sign function,
// so a local end-to-end payment can be completed without reimplementing the
// HMAC scheme and getting it subtly wrong.
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	mockprovider "telemed/internal/domain/payment/payment/provider/mock"
)

func main() {
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	secret := os.Getenv("MOCK_WEBHOOK_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr,
			"signhook: set MOCK_WEBHOOK_SECRET to the value the running\n"+
				"payment-service loaded (see its .env). Refusing to guess: a\n"+
				"wrong secret produces a signature that fails for a reason the\n"+
				"error message will not make obvious.")
		os.Exit(2)
	}
	fmt.Print(mockprovider.Sign(secret, time.Now(), body))
}

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/domain/user/user"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

// Subcommands exist because this binary ships in a `scratch` image.
//
// There is no shell, no curl and no wget in it, deliberately -- so a compose
// healthcheck has nothing to run except this binary, and `gen-secrets.sh` has
// nothing to sign a token with except this binary. Both are one small branch
// here rather than a second image or a busybox smuggled into the final stage.
//
// Returns handled=false when os.Args names no subcommand, which is the normal
// server path.
func runSubcommand(args []string) (exitCode int, handled bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case "healthcheck":
		return healthcheck(), true
	case "mint-service-token":
		return mintServiceToken(args[2:]), true
	case "generate-slots":
		return generateSlots(args[2:]), true
	default:
		return 0, false
	}
}

// healthcheck probes this process's own readiness endpoint over loopback.
//
// /health/ready and not /health/live: live says the process is running, ready
// says its database pools and Redis actually answered, and a container that is
// up but cannot reach Postgres should not be receiving traffic.
func healthcheck() int {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}
	if _, err := strconv.Atoi(port); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP_PORT %q is not a port\n", port)
		return 2
	}

	client := &http.Client{Timeout: 3 * time.Second}
	// The host is the loopback literal and only the port comes from the
	// environment -- and that environment is the container's own, set by the
	// operator, not by a request. gosec's SSRF check (G704) cannot see that the
	// destination is pinned to 127.0.0.1, so it is silenced here rather than
	// restructured into something less clear.
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/health/ready"
	resp, err := client.Get(url) //nolint:noctx,gosec // the client timeout is the deadline; G704: loopback only, see above
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s\n", resp.Status)
		return 1
	}
	return 0
}

// mintServiceToken prints a long-lived mesh token to stdout.
//
//	/server mint-service-token <service-name> [ttl]
//
// It reads the same JWT_PRIVATE_KEY_PEM / JWT_KEY_ID / JWT_ISSUER /
// JWT_AUDIENCE the user domain signs access tokens with, so the token it
// prints verifies against the JWKS this platform already publishes and needs
// no new trust anywhere.
//
// Nothing is logged and nothing else is written to stdout: the output is meant
// to be captured straight into a secrets file.
func mintServiceToken(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: mint-service-token <service-name> [ttl, default 8760h]")
		return 2
	}
	name := args[0]

	ttl := 8760 * time.Hour // one year
	if len(args) > 1 {
		parsed, err := time.ParseDuration(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "mint-service-token: bad ttl %q: %v\n", args[1], err)
			return 2
		}
		ttl = parsed
	}

	pemKey := os.Getenv("JWT_PRIVATE_KEY_PEM")
	if pemKey == "" {
		fmt.Fprintln(os.Stderr, "mint-service-token: JWT_PRIVATE_KEY_PEM is empty. "+
			"It must be the SAME key the user domain runs with, or the token this "+
			"prints will not verify against the platform's published JWKS.")
		return 2
	}

	issuer := envOr("JWT_ISSUER", "telemed-user-service")
	audience := envOr("JWT_AUDIENCE", "telemed-api")
	keyID := envOr("JWT_KEY_ID", "user-service-key-1")

	ti, err := user.NewTokenIssuer(pemKey, keyID, issuer, audience)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint-service-token: %v\n", err)
		return 1
	}
	token, expiresAt, err := ti.IssueServiceToken(name, ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mint-service-token: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "service token for %q, issuer %q, expires %s\n",
		name, issuer, expiresAt.Format(time.RFC3339))
	fmt.Println(token)
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// generateSlots materialises future slots for a given doctor or all active doctors.
//
//	/server generate-slots [doctor-id | all]
//
// If doctor-id is provided, it generates slots for that doctor. If omitted or
// "all", it generates slots for all active doctors who have working hours.
func generateSlots(args []string) int {
	var doctorID *uuid.UUID
	if len(args) > 0 && args[0] != "" && args[0] != "all" {
		parsed, err := uuid.Parse(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate-slots: invalid doctor id %q: %v\n", args[0], err)
			return 2
		}
		doctorID = &parsed
	}

	dsn := os.Getenv("TELEMED_DB_URL_SCHEDULING")
	if dsn == "" {
		dsn = os.Getenv("SCHEDULING_DATABASE_URL")
	}
	baseDSN := os.Getenv("DATABASE_URL")
	if dsn == "" && baseDSN != "" {
		if u, err := url.Parse(baseDSN); err == nil {
			password, _ := u.User.Password()
			u.User = url.UserPassword("telemed_scheduling_app", password)
			q := u.Query()
			q.Set("search_path", database.SearchPathFor("scheduling"))
			u.RawQuery = q.Encode()
			dsn = u.String()
		}
	}
	if dsn == "" && baseDSN == "" {
		fmt.Fprintln(os.Stderr, "generate-slots: no database DSN found (set DATABASE_URL or TELEMED_DB_URL_SCHEDULING)")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log := zerolog.New(os.Stderr).With().Timestamp().Logger()
	targetDSN := dsn
	if targetDSN == "" {
		targetDSN = baseDSN
	}

	pool, err := database.Connect(ctx, database.Config{
		URL:         targetDSN,
		AppName:     "telemed-generate-slots",
		SearchPath:  database.SearchPathFor("scheduling"),
		MaxConns:    4,
		MinConns:    1,
		MaxConnLife: time.Hour,
	}, log)
	if err != nil {
		if baseDSN != "" && targetDSN != baseDSN {
			log.Warn().Msg("connect as telemed_scheduling_app failed; retrying with raw DATABASE_URL")
			pool, err = database.Connect(ctx, database.Config{
				URL:         baseDSN,
				AppName:     "telemed-generate-slots",
				SearchPath:  database.SearchPathFor("scheduling"),
				MaxConns:    4,
				MinConns:    1,
				MaxConnLife: time.Hour,
			}, log)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate-slots: connect database: %v\n", err)
			return 1
		}
	}
	defer pool.Close()

	loc, err := time.LoadLocation(envOr("SCHEDULING_TIMEZONE", "Asia/Colombo"))
	if err != nil {
		loc = time.UTC
	}

	svc := scheduling.NewService(scheduling.Options{
		Pool:       pool,
		Repository: scheduling.NewRepository(),
		Locker:     scheduling.NoopLocker{},
		Queue:      scheduling.NoopWaitlistQueue{},
		Outbox:     events.NewOutbox(serviceName),
		Clock:      scheduling.SystemClock{},
		Location:   loc,
		Logger:     log,
		Metrics:    scheduling.NewMetrics(nil),
	})

	if doctorID != nil {
		res, err := svc.GenerateForDoctor(ctx, *doctorID)
		if err != nil {
			if errors.Is(err, scheduling.ErrDoctorNotConfigured) {
				fmt.Fprintf(os.Stderr, "generate-slots: doctor %s is not configured (no working hours)\n", doctorID)
				return 1
			}
			fmt.Fprintf(os.Stderr, "generate-slots: doctor %s failed: %v\n", doctorID, err)
			return 1
		}
		fmt.Printf("Generated slots for doctor %s: planned=%d inserted=%d from=%s to=%s\n",
			doctorID, res.Planned, res.Inserted, res.From, res.To)
		return 0
	}

	doctors, inserted, err := svc.GenerateAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate-slots: generate all failed: %v\n", err)
		return 1
	}
	fmt.Printf("Generated slots for %d doctors: inserted=%d\n", doctors, inserted)
	return 0
}


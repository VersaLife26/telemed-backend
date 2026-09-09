package main

import (
	"math"
	"testing"
	"time"
)

// LIVEKIT_EMPTY_ROOM_TIMEOUT is an operator-set int that is handed to LiveKit
// as a uint32. Before this was bounded, a negative value wrapped to ~136 years
// and silently disabled LiveKit's own teardown of an abandoned room -- which is
// one of the paths that ends a consultation nobody is in. The conversion must
// be total and the misconfiguration must fail boot, not be absorbed.
func TestEmptyRoomTimeoutSecondsCannotWrap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		seconds int
		want    uint32
	}{
		{"the configured default", 300, 300},
		{"zero means livekit's own default", 0, 0},
		{"negative one must not become 4294967295", -1, 0},
		{"a large negative must not become a huge timeout", math.MinInt32, 0},
		{"above the ceiling is clamped, never wrapped", maxEmptyRoomTimeoutSeconds + 1, maxEmptyRoomTimeoutSeconds},
		{"maxint must not wrap", math.MaxInt64, maxEmptyRoomTimeoutSeconds},
		{"the ceiling itself is allowed", maxEmptyRoomTimeoutSeconds, maxEmptyRoomTimeoutSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := emptyRoomTimeoutSeconds(tc.seconds); got != tc.want {
				t.Fatalf("emptyRoomTimeoutSeconds(%d) = %d, want %d", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestValidateVideoConfigRejectsAnUnusableEmptyRoomTimeout(t *testing.T) {
	t.Parallel()

	base := func(seconds int) Config {
		var c Config
		c.VideoProvider = "mock"
		c.LiveKitTokenTTL = 5 * time.Minute
		c.LiveKitEmptyRoomTimeout = seconds
		return c
	}

	cases := []struct {
		name    string
		seconds int
		wantErr bool
	}{
		{"the default boots", 300, false},
		{"zero boots", 0, false},
		{"the ceiling boots", maxEmptyRoomTimeoutSeconds, false},
		{"negative refuses to boot", -1, true},
		{"absurdly large refuses to boot", maxEmptyRoomTimeoutSeconds + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateVideoConfig(base(tc.seconds))
			if tc.wantErr && err == nil {
				t.Fatalf("LIVEKIT_EMPTY_ROOM_TIMEOUT=%d was accepted; it must fail boot", tc.seconds)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("LIVEKIT_EMPTY_ROOM_TIMEOUT=%d rejected: %v", tc.seconds, err)
			}
		})
	}
}

// The mock video provider authenticates POST /webhooks/livekit with a bearer
// token compared against LIVEKIT_API_SECRET, whose default is published in
// .env.example -- and if that is unset too it falls back to a literal in
// mock_provider.go. That webhook force-ends consultations and writes
// recording_url straight from the body, and the room name is derivable from
// an appointment id. VIDEO_PROVIDER defaulting to mock when unset is what made
// this a live risk rather than a theoretical one.
func TestValidateVideoConfigRefusesTheMockProviderInProduction(t *testing.T) {
	t.Parallel()

	cfg := func(env, provider string) Config {
		var c Config
		c.Env = env
		c.VideoProvider = provider
		c.LiveKitEmptyRoomTimeout = 300
		// wss, not ws: this test is about which PROVIDER is allowed in
		// production, and a cleartext signalling URL is now independently
		// refused outside development. Leaving it as ws would make these cases
		// pass or fail for the wrong reason.
		c.LiveKitURL = "wss://livekit.example.lk"
		c.LiveKitAPIKey = "key"
		c.LiveKitAPISecret = "secret"
		return c
	}

	cases := []struct {
		env      string
		provider string
		wantErr  bool
	}{
		{"dev", "mock", false},
		{"dev", "", false},
		{"staging", "mock", false},
		{"prod", "mock", true},
		{"production", "mock", true},
		{"prod", "", true},
		{"prod", "MOCK", true},
		{"prod", "  mock  ", true},
		// Env spelling must not be a way round it.
		{"Production", "mock", true},
		{"PROD", "mock", true},
		{"live", "mock", true},
		// The real provider is always fine.
		{"prod", "livekit", false},
		{"production", "livekit", false},
	}

	for _, tc := range cases {
		t.Run(tc.env+"/"+tc.provider, func(t *testing.T) {
			t.Parallel()
			err := validateVideoConfig(cfg(tc.env, tc.provider))
			if tc.wantErr && err == nil {
				t.Fatalf("ENV=%q VIDEO_PROVIDER=%q booted; the mock webhook is authenticated by a published default token", tc.env, tc.provider)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ENV=%q VIDEO_PROVIDER=%q refused: %v", tc.env, tc.provider, err)
			}
		})
	}
}

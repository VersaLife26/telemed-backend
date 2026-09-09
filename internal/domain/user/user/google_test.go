package user

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}
}

func TestGoogleTokenInfoVerifier_AcceptsVerifiedToken(t *testing.T) {
	const clientID = "web.apps.googleusercontent.com"
	v := NewGoogleTokenInfoVerifier(clientID, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("id_token") != "good-token" {
			t.Fatalf("id_token query = %q", r.URL.Query().Get("id_token"))
		}
		return jsonResponse(200, `{
			"aud":"`+clientID+`",
			"iss":"https://accounts.google.com",
			"sub":"google-sub-1",
			"email":"Ada@Example.lk",
			"email_verified":"true",
			"name":"Ada Perera"
		}`), nil
	}))

	id, err := v.Verify(t.Context(), "good-token")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Sub != "google-sub-1" || id.Email != "ada@example.lk" || id.Name != "Ada Perera" || !id.EmailVerified {
		t.Fatalf("identity = %+v", id)
	}
}

func TestGoogleTokenInfoVerifier_RejectsWrongAudience(t *testing.T) {
	v := NewGoogleTokenInfoVerifier("expected-client", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{
			"aud":"other-client",
			"iss":"https://accounts.google.com",
			"sub":"x",
			"email":"a@b.lk",
			"email_verified":true
		}`), nil
	}))
	if _, err := v.Verify(t.Context(), "tok"); !errors.Is(err, ErrGoogleTokenInvalid) {
		t.Fatalf("got %v, want ErrGoogleTokenInvalid", err)
	}
}

func TestGoogleTokenInfoVerifier_RejectsUnverifiedEmail(t *testing.T) {
	v := NewGoogleTokenInfoVerifier("client", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{
			"aud":"client",
			"iss":"accounts.google.com",
			"sub":"x",
			"email":"a@b.lk",
			"email_verified":false
		}`), nil
	}))
	if _, err := v.Verify(t.Context(), "tok"); !errors.Is(err, ErrGoogleEmailUnverified) {
		t.Fatalf("got %v, want ErrGoogleEmailUnverified", err)
	}
}

func TestGoogleTokenInfoVerifier_HTTPError(t *testing.T) {
	v := NewGoogleTokenInfoVerifier("client", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, `{"error":"invalid_token"}`), nil
	}))
	if _, err := v.Verify(t.Context(), "tok"); !errors.Is(err, ErrGoogleTokenInvalid) {
		t.Fatalf("got %v, want ErrGoogleTokenInvalid", err)
	}
}

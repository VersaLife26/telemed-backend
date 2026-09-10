package signal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Errors returned by VerifyToken.
var (
	ErrTokenMalformed = errors.New("signal: token is malformed")
	ErrTokenSignature = errors.New("signal: token signature does not verify")
	ErrTokenExpired   = errors.New("signal: token has expired")
)

// Token is what a verified room token grants: this identity, in this room,
// until this instant. Nothing else -- there are no roles and no capabilities
// here, because the only thing a signalling socket can do is talk to the one
// other peer in its own room.
type Token struct {
	Room     string
	Identity string
	Expires  time.Time
}

// tokenParts is the number of dot-separated fields in a minted token.
const tokenParts = 4

// MintToken returns a room-scoped, time-limited bearer token.
//
// It is a compact HMAC rather than a JWT deliberately. A JWT would invite the
// question of which claims to honour, and the honest answer is none of them:
// the room and the identity are the entire grant, and both are bound by the
// signature. Room and identity are base64url-encoded before joining so a room
// name containing a dot cannot shift the field boundaries and let one room's
// token verify against another's.
func MintToken(secret []byte, room, identity string, ttl time.Duration) string {
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	body := strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(room)),
		base64.RawURLEncoding.EncodeToString([]byte(identity)),
		exp,
	}, ".")
	return body + "." + sign(secret, body)
}

// VerifyToken checks a token's signature and expiry and returns what it grants.
//
// The signature is checked BEFORE the expiry, which is the order that matters:
// checked the other way round, an attacker learns whether a forged token's
// timestamp was in the past without ever having to guess the key.
func VerifyToken(secret []byte, token string) (Token, error) {
	parts := strings.Split(token, ".")
	if len(parts) != tokenParts {
		return Token{}, ErrTokenMalformed
	}
	body := strings.Join(parts[:tokenParts-1], ".")
	if !hmac.Equal([]byte(parts[tokenParts-1]), []byte(sign(secret, body))) {
		return Token{}, ErrTokenSignature
	}

	room, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Token{}, ErrTokenMalformed
	}
	identity, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Token{}, ErrTokenMalformed
	}
	unix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Token{}, ErrTokenMalformed
	}
	exp := time.Unix(unix, 0).UTC()
	if time.Now().After(exp) {
		return Token{}, fmt.Errorf("%w at %s", ErrTokenExpired, exp.Format(time.RFC3339))
	}
	return Token{Room: string(room), Identity: string(identity), Expires: exp}, nil
}

func sign(secret []byte, body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

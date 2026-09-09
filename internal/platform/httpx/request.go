package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// maxBodyBytes caps request bodies at 1 MiB. File uploads use their own,
// larger limit at the multipart handler; this default protects every JSON
// endpoint from a trivially cheap memory-exhaustion attempt.
const maxBodyBytes = 1 << 20

var (
	validateOnce sync.Once
	validate     *validator.Validate

	// sriLankanMobile matches the E.164 form of a Sri Lankan mobile number.
	// Mobile prefixes are 07X locally, which is +947X internationally.
	sriLankanMobile = regexp.MustCompile(`^\+947\d{8}$`)

	// slmcNumber matches a Sri Lanka Medical Council registration number.
	//
	// The number itself is digits. Two prefixes are accepted: an optional
	// literal "SLMC", which is what doctors and our own seed data habitually
	// type in front of it, and an optional short register code of up to three
	// letters. Being explicit about those two beats simply widening the letter
	// bound -- "XYZQ1234" should still be rejected, and a bare {0,5} would let
	// it through.
	slmcNumber = regexp.MustCompile(`^(SLMC)?[A-Z]{0,3}\d{4,8}$`)
)

// Validator returns the process-wide validator with the telemed custom rules
// registered. It is safe for concurrent use.
func Validator() *validator.Validate {
	validateOnce.Do(func() {
		validate = validator.New(validator.WithRequiredStructEnabled())
		_ = validate.RegisterValidation("sriphone", func(fl validator.FieldLevel) bool {
			return sriLankanMobile.MatchString(fl.Field().String())
		})
		_ = validate.RegisterValidation("slmc", func(fl validator.FieldLevel) bool {
			return slmcNumber.MatchString(strings.ToUpper(fl.Field().String()))
		})
	})
	return validate
}

// NormalizePhone converts the shapes Sri Lankan users actually type --
// 0771234567, 771234567, 94771234567, +94 77 123 4567 -- into E.164.
// It returns an empty string when the input cannot be a Sri Lankan mobile.
func NormalizePhone(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case strings.HasPrefix(d, "94") && len(d) == 11: // 94771234567
		d = d[2:]
	case strings.HasPrefix(d, "0") && len(d) == 10: // 0771234567
		d = d[1:]
	}
	if len(d) != 9 || d[0] != '7' {
		return ""
	}
	return "+94" + d
}

// DecodeJSON reads, size-limits, strictly decodes, and validates a request body.
// Unknown fields are rejected: a client sending "amount_lkr" when the server
// expects "amount" should get a clear 400, not a silent zero.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return NewError(http.StatusUnsupportedMediaType, CodeBadRequest, "Content-Type must be application/json")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &syntaxErr):
			return ErrBadRequest.WithCause(err)
		case errors.As(err, &typeErr):
			return &APIError{
				Code:    CodeBadRequest,
				Message: "field " + typeErr.Field + " has the wrong type",
				status:  http.StatusBadRequest,
				cause:   err,
			}
		case errors.As(err, &maxErr):
			return NewError(http.StatusRequestEntityTooLarge, CodeBadRequest, "request body too large")
		case errors.Is(err, io.EOF):
			return NewError(http.StatusBadRequest, CodeBadRequest, "request body is empty")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			field := strings.TrimPrefix(err.Error(), "json: unknown field ")
			return &APIError{
				Code:    CodeBadRequest,
				Message: "unknown field " + field,
				status:  http.StatusBadRequest,
				cause:   err,
			}
		default:
			return ErrBadRequest.WithCause(err)
		}
	}

	// A second Decode must hit EOF; anything else means the client sent two
	// JSON documents in one body.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return NewError(http.StatusBadRequest, CodeBadRequest, "body must contain a single JSON object")
	}

	if err := Validator().Struct(dst); err != nil {
		return ValidationError(err)
	}
	return nil
}

// PathUUID parses a UUID path parameter, returning a 400 rather than letting a
// malformed value reach the database layer.
func PathUUID(r *http.Request, name string, param func(*http.Request, string) string) (uuid.UUID, error) {
	raw := param(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &APIError{
			Code:    CodeBadRequest,
			Message: name + " must be a valid UUID",
			status:  http.StatusBadRequest,
			cause:   err,
		}
	}
	return id, nil
}

// MaxPage caps the page number.
//
// per_page was capped and page was not, and offset is (page-1)*perPage in int
// arithmetic -- so ?page=92233720368547758&per_page=100 wrapped to a NEGATIVE
// offset, which Postgres rejects with "OFFSET must not be negative": a 500 on
// every list endpoint on the platform, from a query string. Values below the
// wrap point produced a huge but harmless offset.
//
// 100,000 pages at the 100-row cap is ten million rows deep, which is past
// anything a human or a sensible client asks for and well clear of the wrap.
const MaxPage = 100_000

// Pagination reads page/per_page query parameters with sane, capped defaults.
// Both are clamped, so the returned offset is always a small non-negative int.
func Pagination(r *http.Request) (page, perPage, offset int) {
	page = queryInt(r, "page", 1)
	switch {
	case page < 1:
		page = 1
	case page > MaxPage:
		page = MaxPage
	}
	perPage = queryInt(r, "per_page", 20)
	switch {
	case perPage < 1:
		perPage = 20
	case perPage > 100:
		perPage = 100 // a hard cap keeps one client from pulling the whole table
	}
	return page, perPage, (page - 1) * perPage
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// QueryUUID parses an optional UUID query parameter.
func QueryUUID(r *http.Request, key string) (uuid.UUID, bool, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return uuid.Nil, false, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false, NewError(http.StatusBadRequest, CodeBadRequest, key+" must be a valid UUID")
	}
	return id, true, nil
}

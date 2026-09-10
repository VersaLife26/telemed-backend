package storage

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// maxPresignedUpload caps a presigned PUT.
//
// The route is unauthenticated by design -- the signature IS the authorisation
// -- so it needs its own body limit rather than inheriting one from a
// middleware chain that assumes a principal. 15 MiB matches the platform's
// upload class (MAX_BODY_BYTES_UPLOAD).
const maxPresignedUpload = 15 << 20

// PresignHandler serves the URLs FilesystemStorage.PresignedGet and
// PresignedPut hand out.
//
// It is deliberately NOT behind RequireAuth. A presigned URL is a bearer
// capability: the HMAC over bucket, key, operation and expiry is what
// authorises the request, exactly as it is for S3. Putting an auth check in
// front would break the one thing these URLs exist for -- being handed to a
// browser, or to a client that has no session.
//
// Two consequences follow and are handled below rather than assumed away:
//
//   - The operation is bound into the signature, so a GET URL cannot be
//     replayed as a PUT. The handler re-checks that the HTTP method matches
//     the signed operation, because VerifyPresigned returns the op rather
//     than enforcing it.
//   - Nothing here consults the medical-records access log. These URLs are
//     minted by a handler that already made and logged that decision; this
//     endpoint only honours a decision already taken, which is why the TTLs
//     that mint them are minutes rather than hours.
func PresignHandler(fs *FilesystemStorage) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, key, op, err := fs.VerifyPresigned(r.URL.RawQuery)
		if err != nil {
			// One status and one message for a bad signature, an expired URL
			// and a malformed query alike. Distinguishing them tells a holder
			// which part of a forgery attempt to vary.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		switch {
		case r.Method == http.MethodGet && op == "GET":
			servePresignedGet(w, r, fs, bucket, key)
		case r.Method == http.MethodPut && op == "PUT":
			servePresignedPut(w, r, fs, bucket, key)
		case r.Method == http.MethodHead && op == "GET":
			servePresignedHead(w, r, fs, bucket, key)
		default:
			// A signature for one operation presented for another.
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func servePresignedGet(w http.ResponseWriter, r *http.Request, fs *FilesystemStorage, bucket, key string) {
	info, err := fs.Stat(r.Context(), bucket, key)
	if err != nil {
		writeObjectError(w, err)
		return
	}
	body, err := fs.Get(r.Context(), bucket, key)
	if err != nil {
		writeObjectError(w, err)
		return
	}
	defer func() { _ = body.Close() }()

	setObjectHeaders(w, info)
	// No Content-Length: on a short read the client would otherwise wait for
	// bytes that are not coming. Go's chunked encoding ends the response
	// honestly instead.
	if _, err := io.Copy(w, body); err != nil {
		// The status line is already sent, so there is nothing to report to
		// the client. Returning is the whole remedy.
		return
	}
}

func servePresignedHead(w http.ResponseWriter, r *http.Request, fs *FilesystemStorage, bucket, key string) {
	info, err := fs.Stat(r.Context(), bucket, key)
	if err != nil {
		writeObjectError(w, err)
		return
	}
	setObjectHeaders(w, info)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func servePresignedPut(w http.ResponseWriter, r *http.Request, fs *FilesystemStorage, bucket, key string) {
	defer func() { _ = r.Body.Close() }()
	body := http.MaxBytesReader(w, r.Body, maxPresignedUpload)

	// -1 for the size: Put treats a non-negative size as an assertion to
	// verify, and Content-Length on an upload is whatever the client said.
	if err := fs.Put(r.Context(), bucket, key, body, -1, r.Header.Get("Content-Type")); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not store object", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func setObjectHeaders(w http.ResponseWriter, info ObjectInfo) {
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	// These objects are medical records reached through a short-lived URL.
	// An intermediary caching one would outlive the capability that granted
	// it, which is the entire point of the expiry.
	w.Header().Set("Cache-Control", "no-store, private")
	// The bytes are user-supplied. Rendering them inline is how an uploaded
	// .html or .svg becomes stored XSS on the API's own origin.
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if info.ETag != "" {
		w.Header().Set("ETag", fmt.Sprintf("%q", info.ETag))
	}
}

func writeObjectError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "could not read object", http.StatusInternalServerError)
}

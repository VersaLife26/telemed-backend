package gateway

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The OpenAPI document is what the TypeScript Next.js teams generate their
// clients from. If it disagrees with the route table, a generated client calls
// a path the gateway does not route -- and the mistake surfaces as a 404 in an
// app, weeks later, rather than here.
//
// So: every route in the table must appear in the document, and every
// documented path must be routed. This test is the reason the two can be
// trusted to describe the same API.

// openapiOperations extracts "METHOD /path" from the embedded document.
//
// It is a line scanner, not a YAML parse, deliberately: pulling a YAML
// dependency into this package for one test would put a parser on the module
// graph of a service whose whole job is to forward bytes. The document's shape
// is fixed and machine-written -- path keys at two spaces, method keys at four
// -- and `make openapi-sync` keeps the embedded copy byte-identical to the one
// on disk, so the scan is exact for the file it is scanning.
func openapiOperations(t *testing.T) map[string]struct{} {
	t.Helper()

	methods := map[string]struct{}{
		"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {}, "head": {}, "options": {},
	}

	ops := map[string]struct{}{}
	inPaths := false
	current := ""

	sc := bufio.NewScanner(bytes.NewReader(openapiYAML))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		// A non-indented, non-empty line ends the paths block.
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2 && strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":"):
			current = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && current != "" && strings.HasSuffix(trimmed, ":"):
			key := strings.TrimSuffix(trimmed, ":")
			if _, ok := methods[key]; ok {
				ops[strings.ToUpper(key)+" "+current] = struct{}{}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan openapi document: %v", err)
	}
	if len(ops) == 0 {
		t.Fatal("no operations found in the embedded OpenAPI document -- the scanner or the document changed shape")
	}
	return ops
}

// TestOpenAPI_DocumentsEveryRoutedOperation. A route the apps can call but
// that no client generator knows about is a feature nobody uses.
func TestOpenAPI_DocumentsEveryRoutedOperation(t *testing.T) {
	ops := openapiOperations(t)

	var missing []string
	for _, r := range loadTestRoutes(t) {
		if strings.HasSuffix(r.Pattern, "*") {
			// The admin catchall is documented in prose on the `admin` tag,
			// because the source docs name no concrete paths behind it.
			continue
		}
		if isTestSurface(r) {
			// Deliberately undocumented. openapi.yaml is the contract the
			// patient, doctor and admin clients are generated from and the
			// document published to anyone integrating with the platform; the
			// test surface is neither. Documenting an unauthenticated endpoint
			// that returns plaintext OTP codes would advertise it to every
			// consumer of the spec and invite a generated client to call it,
			// while the surface itself does not exist in any environment those
			// clients talk to.
			continue
		}
		methods := r.Methods
		if len(methods) == 0 {
			methods = []string{"GET"}
		}
		for _, m := range methods {
			key := m + " " + r.Pattern
			if _, ok := ops[key]; !ok {
				missing = append(missing, fmt.Sprintf("%s  (route %q)", key, r.Name))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d routed operation(s) are absent from openapi.yaml; add them and run "+
			"`make openapi-sync`:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestOpenAPI_DocumentsNothingUnrouted is the other direction, and the one
// that actually bites: a generated client calls a documented path, the gateway
// has no rule for it, and the app gets a 404 it cannot explain.
func TestOpenAPI_DocumentsNothingUnrouted(t *testing.T) {
	routed := map[string]struct{}{}
	for _, r := range loadTestRoutes(t) {
		methods := r.Methods
		if len(methods) == 0 {
			methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
		}
		for _, m := range methods {
			routed[m+" "+r.Pattern] = struct{}{}
		}
	}

	var extra []string
	for op := range openapiOperations(t) {
		if _, ok := routed[op]; !ok {
			extra = append(extra, op)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("%d documented operation(s) have no route; a generated client would 404 "+
			"on them:\n  %s", len(extra), strings.Join(extra, "\n  "))
	}
}

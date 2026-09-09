package gateway

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// refPattern matches every internal component reference in the document.
var refPattern = regexp.MustCompile(`#/components/([A-Za-z0-9_]+)/([A-Za-z0-9_]+)`)

// componentNames returns the names defined under components/<section>.
//
// A line-scanner rather than a YAML unmarshal, for the same reason
// openapiOperations uses one: the document is the artefact under test, and
// parsing it into a permissive map would let a structural change pass while
// the file a client actually reads is broken.
func componentNames(t *testing.T) map[string]map[string]bool {
	t.Helper()

	defined := map[string]map[string]bool{}
	var section string
	inComponents := false

	for _, line := range strings.Split(string(openapiYAML), "\n") {
		if strings.HasPrefix(line, "components:") {
			inComponents = true
			continue
		}
		if !inComponents {
			continue
		}
		// A non-indented, non-empty line ends the components block.
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		trimmed := strings.TrimRight(line, " ")
		if trimmed == "" {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		name := strings.TrimSuffix(strings.TrimSpace(trimmed), ":")
		switch indent {
		case 2: // components/<section>
			section = name
			if _, ok := defined[section]; !ok {
				defined[section] = map[string]bool{}
			}
		case 4: // components/<section>/<Name>
			if section != "" && !strings.Contains(trimmed, ": ") {
				defined[section][name] = true
			}
		}
	}
	if len(defined) == 0 {
		t.Fatal("no components found -- the document or this scanner changed shape")
	}
	return defined
}

// TestOpenAPI_HasNoDanglingRefs fails when the document references a component
// it does not define.
//
// The existing contract tests compare the document against the ROUTE TABLE, so
// they are blind to this: a path can be present, correctly routed, and still
// point at '#/components/responses/Unavailable' when the definition is called
// ServiceUnavailable. The result is a spec that generators reject and that
// silently misdescribes the API to anyone reading it -- and three such refs
// were live in this document before this test existed, two of them predating
// the route that exposed them.
func TestOpenAPI_HasNoDanglingRefs(t *testing.T) {
	defined := componentNames(t)

	seen := map[string]bool{}
	var dangling []string
	for _, m := range refPattern.FindAllStringSubmatch(string(openapiYAML), -1) {
		section, name := m[1], m[2]
		key := section + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		if !defined[section][name] {
			dangling = append(dangling, key)
		}
	}

	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Fatalf("%d dangling $ref(s) in the OpenAPI document:\n  %s\n"+
			"Every #/components/<section>/<Name> must resolve. Check for a name "+
			"that is close but not equal to a real one.",
			len(dangling), strings.Join(dangling, "\n  "))
	}
}

// TestOpenAPI_DefinesTheResponsesItAdvertises is a narrower canary on the
// handful of shared responses most paths reuse, so a rename shows up here by
// name rather than as a list of paths.
func TestOpenAPI_DefinesTheResponsesItAdvertises(t *testing.T) {
	defined := componentNames(t)
	for _, name := range []string{
		"Unauthorized", "Forbidden", "NotFound", "RateLimited",
		"ServiceUnavailable", "Conflict", "OK",
	} {
		if !defined["responses"][name] {
			t.Errorf("components/responses/%s is not defined, but paths reference it", name)
		}
	}
}

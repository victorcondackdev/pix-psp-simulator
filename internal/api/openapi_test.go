package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

type spec struct {
	OpenAPI string                                `json:"openapi"`
	Paths   map[string]map[string]json.RawMessage `json:"paths"`
}

// specPath converts a ServeMux pattern to an OpenAPI path.
func specPath(pattern string) string {
	return strings.ReplaceAll(pattern, "{$}", "")
}

// Every registered route is documented, and every documented operation is
// registered, so the spec cannot drift from the mux.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var doc spec
	if err := json.Unmarshal(openapiJSON, &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q", doc.OpenAPI)
	}
	srv := New(nil, nil, nil)
	registered := map[string]bool{}
	for _, r := range srv.Routes() {
		key := strings.ToLower(r.Method) + " " + specPath(r.Pattern)
		if registered[key] {
			t.Errorf("route registered twice: %s", key)
		}
		registered[key] = true
		ops, ok := doc.Paths[specPath(r.Pattern)]
		if !ok {
			t.Errorf("path %s (%s) missing from openapi.json", specPath(r.Pattern), r.Pattern)
			continue
		}
		if _, ok := ops[strings.ToLower(r.Method)]; !ok {
			t.Errorf("operation %s %s missing from openapi.json", r.Method, specPath(r.Pattern))
		}
	}
	var extra []string
	for path, ops := range doc.Paths {
		for method := range ops {
			if method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			if !registered[method+" "+path] {
				extra = append(extra, method+" "+path)
			}
		}
	}
	sort.Strings(extra)
	for _, e := range extra {
		t.Errorf("openapi.json documents %s, which is not registered", e)
	}
}

// Every $ref points to an existing component and operation IDs are unique.
func TestOpenAPIRefsResolve(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(openapiJSON, &doc); err != nil {
		t.Fatal(err)
	}
	components := doc["components"].(map[string]any)
	ids := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
				if len(parts) != 2 {
					t.Errorf("unexpected ref %s", ref)
				} else if sect, ok := components[parts[0]].(map[string]any); !ok || sect[parts[1]] == nil {
					t.Errorf("dangling ref %s", ref)
				}
			}
			if id, ok := x["operationId"].(string); ok {
				if ids[id] {
					t.Errorf("duplicate operationId %s", id)
				}
				ids[id] = true
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	if len(ids) == 0 {
		t.Fatal("no operations found")
	}
}

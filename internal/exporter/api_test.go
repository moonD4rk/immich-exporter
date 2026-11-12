package exporter

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// envOpenAPI names a copy of Immich's OpenAPI spec, open-api/immich-openapi-specs.json in immich-app/immich.
const envOpenAPI = "IMMICH_OPENAPI_SPEC"

// apiSurface is what the exporter asks of Immich: every route with its query parameters, and the search fields.
type apiSurface struct {
	mu           sync.Mutex
	routes       map[string]map[string]bool
	filter, flat map[string]bool
	unserved     []string
}

// record notes the calls that reach mux, the mock every test polls, so the surface can't drift from the code.
func (a *apiSurface) record(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, route := mux.Handler(r)
		a.mu.Lock()
		if route == "" {
			a.unserved = append(a.unserved, r.Method+" "+r.URL.Path)
		} else {
			if a.routes[route] == nil {
				a.routes[route] = map[string]bool{}
			}
			for name := range r.URL.Query() {
				a.routes[route][name] = true
			}
		}
		if route == "POST /search/statistics" {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			filter, _ := body["filter"].(map[string]any)
			delete(body, "filter")
			maps.Insert(a.filter, maps.All(keySet(filter)))
			maps.Insert(a.flat, maps.All(keySet(body)))
		}
		a.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func keySet(m map[string]any) map[string]bool {
	set := map[string]bool{}
	for k := range m {
		set[k] = true
	}
	return set
}

// exporterSurface polls the mock as a current server, then as an Immich 3.1 without the routes that have a fallback.
func exporterSurface(t *testing.T) *apiSurface {
	t.Helper()
	a := &apiSurface{routes: map[string]map[string]bool{}, filter: map[string]bool{}, flat: map[string]bool{}}
	mux := immichMux(true)
	p, _ := testPoller(serve(t, a.record(mux, mux)))
	p.poll(context.Background())

	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/about":
			writeJSON(w, map[string]any{"version": "v3.1.0"})
		case "/server/features", "/server/config", "/queues":
			http.NotFound(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})
	p, _ = testPoller(serve(t, a.record(mux, legacy)))
	p.poll(context.Background())
	return a
}

// Every call must reach a route of the mock: one it doesn't serve is one no test covers.
func TestMockServesEveryCall(t *testing.T) {
	a := exporterSurface(t)
	if len(a.unserved) > 0 {
		t.Errorf("calls the mock does not serve: %v", a.unserved)
	}
	for _, route := range []string{
		"GET /server/ping", "POST /search/statistics", "GET /people/{id}/statistics", "GET /admin/users/{id}/statistics",
		"GET /queues", "GET /jobs", "GET /server/features", "GET /config", "GET /map/markers",
	} {
		if a.routes[route] == nil {
			t.Errorf("%s was not called; routes: %v", route, slices.Sorted(maps.Keys(a.routes)))
		}
	}
	if !a.filter["trashedAt"] || !a.filter["hasAlbums"] || !a.flat["isNotInAlbum"] {
		t.Errorf("search fields not recorded: filter %v, flat %v", a.filter, a.flat)
	}
}

type openAPIParameter struct {
	Name string `json:"name"`
	In   string `json:"in"`
}

type openAPIOperation struct {
	Deprecated bool               `json:"deprecated"`
	Parameters []openAPIParameter `json:"parameters"`
}

type openAPISchema struct {
	Properties map[string]struct {
		Deprecated bool `json:"deprecated"`
	} `json:"properties"`
}

type openAPI struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]jsontext.Value `json:"paths"`
	Components struct {
		Schemas map[string]openAPISchema `json:"schemas"`
	} `json:"components"`
}

// replaced are the routes the exporter has a fallback for, so their deprecation or removal breaks nothing.
var replaced = map[string]string{
	"GET /jobs":            "GET /queues",
	"GET /server/features": "GET /config",
	"GET /server/config":   "GET /config",
}

// unlisted are query parameters sent for servers older than the spec; a current one ignores them.
var unlisted = map[string][]string{"GET /timeline/buckets": {"size"}}

// A lenient Immich answers a renamed parameter with the whole library, not an error; its spec tells before an upgrade does.
func TestImmichAPIMatchesOpenAPI(t *testing.T) {
	path := os.Getenv(envOpenAPI)
	if path == "" {
		t.Skipf("set %s to Immich's OpenAPI spec to check the exporter's API calls against it", envOpenAPI)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the developer's own.
	if err != nil {
		t.Fatal(err)
	}
	var spec openAPI
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	t.Logf("Immich OpenAPI %s", spec.Info.Version)
	a := exporterSurface(t)

	for _, route := range slices.Sorted(maps.Keys(a.routes)) {
		method, apiPath, _ := strings.Cut(route, " ")
		rawOp, ok := spec.Paths[apiPath][strings.ToLower(method)]
		successor, hasFallback := replaced[route]
		if !ok {
			if hasFallback {
				t.Logf("%s is gone; the exporter falls back to %s", route, successor)
			} else {
				t.Errorf("%s is not in the spec: removed or renamed", route)
			}
			continue
		}
		var op openAPIOperation
		if err := json.Unmarshal(rawOp, &op); err != nil {
			t.Errorf("%s: %v", route, err)
			continue
		}
		if op.Deprecated && !hasFallback {
			t.Errorf("%s is deprecated and has no fallback yet", route)
		}
		for _, name := range slices.Sorted(maps.Keys(a.routes[route])) {
			declared := slices.Contains(op.Parameters, openAPIParameter{Name: name, In: "query"})
			if !declared && !slices.Contains(unlisted[route], name) {
				t.Errorf("%s: query parameter %q is not in the spec, so the server would ignore it", route, name)
			}
		}
	}

	search := spec.Components.Schemas["StatisticsSearchDto"].Properties
	if _, ok := search["filter"]; !ok {
		t.Error("StatisticsSearchDto has no filter")
	}
	filter := spec.Components.Schemas["SearchFilter"].Properties
	for _, field := range slices.Sorted(maps.Keys(a.filter)) {
		if p, ok := filter[field]; !ok || p.Deprecated {
			t.Errorf("SearchFilter.%s is gone or deprecated (present=%v)", field, ok)
		}
	}
	// The flat fields only go to servers before 3.2, so their removal from a newer spec is expected.
	for _, field := range slices.Sorted(maps.Keys(a.flat)) {
		if _, ok := search[field]; !ok {
			t.Logf("flat search field %s is gone; servers from 3.2 get the filter", field)
		}
	}
}

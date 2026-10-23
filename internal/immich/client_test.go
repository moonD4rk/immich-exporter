package immich

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendsHeadersAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "secret" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "immich-exporter/test" {
			t.Errorf("User-Agent = %q", got)
		}
		if r.URL.Path != "/api/search/statistics" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil || body["isFavorite"] != true {
			t.Errorf("body = %v (err %v)", body, err)
		}
		_, _ = fmt.Fprint(w, `{"total": 42}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/", "secret", "immich-exporter/test", srv.Client())
	n, err := c.StatTotal(t.Context(), map[string]any{"isFavorite": true})
	if err != nil || n != 42 {
		t.Fatalf("StatTotal = %v, %v; want 42", n, err)
	}
}

func TestClientAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 1000), http.StatusForbidden)
	}))
	defer srv.Close()

	err := New(srv.URL, "k", "", srv.Client()).Get(t.Context(), "/queues", nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ae.Status != http.StatusForbidden || ae.Path != "/queues" || ae.Method != http.MethodGet {
		t.Errorf("APIError = %+v", ae)
	}
	if len(ae.Body) > 256 {
		t.Errorf("error body kept %d bytes, want at most 256", len(ae.Body))
	}
	if !StatusIs(err, 401, 403) || StatusIs(err, 404) {
		t.Error("StatusIs should match 403 only")
	}
	if StatusIs(errors.New("dial tcp: refused"), 403) {
		t.Error("StatusIs must not match a transport error")
	}
}

func TestSuggestDropsBlankValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "camera-lens-model" {
			t.Errorf("type = %q", got)
		}
		_, _ = fmt.Fprint(w, `["24mm", "", "  ", "50mm"]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "k", "", srv.Client()).Suggest(t.Context(), "camera-lens-model")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "24mm,50mm" {
		t.Errorf("Suggest = %q", got)
	}
}

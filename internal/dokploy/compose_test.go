package dokploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestClient_ComposeServices(t *testing.T) {
	srv := fixturePanel(t, map[string]string{
		"compose.loadServices?composeId=cmp_stack&type=cache": "compose.loadServices.json",
	})
	got, err := newClient(t, srv.URL, testKey).ComposeServices(context.Background(), "cmp_stack")
	if err != nil {
		t.Fatalf("ComposeServices: %v", err)
	}
	if want := []string{"grafana", "prometheus"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ComposeServices = %q, want %q", got, want)
	}
}

// type=fetch makes Dokploy git clone the compose source on the server, so
// it must never be sent: only the cached compose file is read.
func TestClient_ComposeServicesNeverFetches(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/compose.loadServices" {
			t.Errorf("request %s %s, want GET /api/compose.loadServices", r.Method, r.URL.Path)
		}
		if got := q["type"]; len(got) != 1 || got[0] != "cache" {
			t.Errorf("type = %q, want exactly [\"cache\"]; never \"fetch\"", got)
		}
		fmt.Fprint(w, `["postgres"]`)
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL, testKey).ComposeServices(context.Background(), "cmp_stack"); err != nil {
		t.Fatalf("ComposeServices: %v", err)
	}
	if calls != 1 {
		t.Errorf("%d requests, want 1", calls)
	}
}

func TestClient_ComposeServicesErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		// checkServicePermissionAndAccess rejects a member without access to
		// the compose service with UNAUTHORIZED.
		{"no access", 401, `{"message":"You don't have access to this service","code":"UNAUTHORIZED"}`, ErrUnauthorized},
		{"forbidden", 403, `{"message":"Forbidden","code":"FORBIDDEN"}`, ErrUnauthorized},
		// loadServices throws NOT_FOUND when no compose file is cached.
		{"no cached services", 404, `{"message":"Services not found","code":"NOT_FOUND"}`, ErrNotFound},
		{"not a list", 200, `{"services":[]}`, ErrUnexpectedResponse},
		{"null", 200, `null`, ErrUnexpectedResponse},
		{"empty name", 200, `["postgres",""]`, ErrUnexpectedResponse},
		{"name with a slash", 200, `["db/postgres"]`, ErrUnexpectedResponse},
		{"name with spaces", 200, `["my postgres"]`, ErrUnexpectedResponse},
		{"duplicate name", 200, `["postgres","postgres"]`, ErrUnexpectedResponse},
		{"server error", 500, `oops`, ErrUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, testKey).ComposeServices(context.Background(), "cmp_stack")
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClient_ComposeServicesEmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL, testKey).ComposeServices(context.Background(), ""); err == nil {
		t.Error("ComposeServices with an empty ID succeeded, want an error")
	}
}

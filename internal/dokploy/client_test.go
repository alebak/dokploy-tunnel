package dokploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "dok_test_key"

// fakePanel serves the two procedures the client calls, the way Dokploy's
// OpenAPI adapter exposes them, for an API key bound to organization org1.
func fakePanel(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/api/user.session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Unauthorized"}`)
			return
		}
		fmt.Fprint(w, `{"user":{"id":"u1"},"session":{"activeOrganizationId":"org1"}}`)
	})
	mux.HandleFunc("GET "+prefix+"/api/organization.one", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("organizationId") != "org1" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You are not a member of this organization","code":"FORBIDDEN"}`)
			return
		}
		fmt.Fprint(w, `{"id":"org1","name":"Acme","slug":"x","ownerId":"u1"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, base, key string) *Client {
	t.Helper()
	u, err := ParseBaseURL(base)
	if err != nil {
		t.Fatalf("ParseBaseURL(%q): %v", base, err)
	}
	return New(u, key)
}

func TestClient_Organization(t *testing.T) {
	for _, prefix := range []string{"", "/dokploy"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			srv := fakePanel(t, prefix)
			org, err := newClient(t, srv.URL+prefix+"/", testKey).Organization(context.Background())
			if err != nil {
				t.Fatalf("Organization: %v", err)
			}
			if want := (Organization{ID: "org1", Name: "Acme"}); org != want {
				t.Errorf("Organization = %+v, want %+v", org, want)
			}
		})
	}
}

func TestClient_OrganizationErrors(t *testing.T) {
	handler := func(sessionBody string, sessionStatus int, orgBody string) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/user.session", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(sessionStatus)
			fmt.Fprint(w, sessionBody)
		})
		mux.HandleFunc("GET /api/organization.one", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, orgBody)
		})
		return mux
	}
	tests := []struct {
		name    string
		handler http.Handler
		want    error
	}{
		{"rejected key", handler(`{"message":"Unauthorized"}`, 401, ""), ErrUnauthorized},
		{"forbidden", handler(`{}`, 403, ""), ErrUnauthorized},
		{"no session for key", handler(`null`, 200, ""), ErrUnauthorized},
		{"server error", handler(`oops`, 500, ""), ErrUnexpectedResponse},
		{"not a Dokploy panel", handler(`<html>`, 200, ""), ErrUnexpectedResponse},
		{"empty organization id", handler(`{"session":{"activeOrganizationId":""}}`, 200, ""), ErrUnauthorized},
		{"organization not returned", handler(`{"session":{"activeOrganizationId":"org1"}}`, 200, `null`), ErrUnexpectedResponse},
		{"other organization returned", handler(`{"session":{"activeOrganizationId":"org1"}}`, 200, `{"id":"org2","name":"Evil"}`), ErrUnexpectedResponse},
		{"organization without name", handler(`{"session":{"activeOrganizationId":"org1"}}`, 200, `{"id":"org1","name":""}`), ErrUnexpectedResponse},
		{"oversized response", handler(`{"session":{"activeOrganizationId":"`+strings.Repeat("a", maxResponseBytes)+`"}}`, 200, ""), ErrUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			_, err := newClient(t, srv.URL, testKey).Organization(context.Background())
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClient_InvalidKeyIsUnauthorized(t *testing.T) {
	srv := fakePanel(t, "")
	_, err := newClient(t, srv.URL, "wrong").Organization(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestClient_UnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	_, err := newClient(t, base, testKey).Organization(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
	if strings.Contains(fmt.Sprint(err), testKey) {
		t.Errorf("error leaks the API key: %v", err)
	}
}

func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-api-key") != ""
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := newClient(t, redirector.URL, testKey).Organization(context.Background())
	if !errors.Is(err, ErrUnexpectedResponse) || !strings.Contains(err.Error(), target.URL) {
		t.Errorf("err = %v, want ErrUnexpectedResponse naming the redirect target", err)
	}
	if leaked {
		t.Error("the API key was sent to the redirect target")
	}
}

func TestParseBaseURL(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"https://dokploy.example.com", "https://dokploy.example.com", false},
		{"https://dokploy.example.com/", "https://dokploy.example.com", false},
		{"http://192.168.1.20:3000", "http://192.168.1.20:3000", false},
		{"HTTP://Panel.Example.com:3000/sub/", "http://Panel.Example.com:3000/sub", false},
		{"  https://dokploy.example.com  ", "https://dokploy.example.com", false},
		{"dokploy.example.com", "", true},
		{"ftp://dokploy.example.com", "", true},
		{"https://", "", true},
		{"https://user:pass@dokploy.example.com", "", true},
		{"https://dokploy.example.com/?x=1", "", true},
		{"https://dokploy.example.com/#frag", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			u, err := ParseBaseURL(tt.raw)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidURL) {
					t.Errorf("err = %v, want ErrInvalidURL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBaseURL: %v", err)
			}
			if u.String() != tt.want {
				t.Errorf("URL = %q, want %q", u, tt.want)
			}
		})
	}
}

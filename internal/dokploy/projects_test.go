package dokploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixturePanel serves recorded Dokploy responses from testdata. routes maps
// "procedure" or "procedure?query" to a fixture file name; any other request,
// or one without the test API key, gets the error Dokploy would send.
func fixturePanel(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("x-api-key") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Unauthorized","code":"UNAUTHORIZED"}`)
			return
		}
		route := r.URL.Path[len("/api/"):]
		if r.URL.RawQuery != "" {
			route += "?" + r.URL.RawQuery
		}
		name, ok := routes[route]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not found","code":"NOT_FOUND"}`)
			return
		}
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("reading fixture: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_Projects(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []Project
	}{
		{
			// Owners and admins get every service, but databases carry only
			// their ID: names and statuses come from the detail calls.
			name:    "owner sees every service type",
			fixture: "project.all.owner.json",
			want: []Project{
				{
					ID: "prj_shop", Name: "shop",
					Environments: []Environment{
						{
							ID: "env_shop_prod", Name: "production", IsDefault: true,
							Services: []Service{
								{ID: "app_web", Type: ServiceApplication, Name: "web", Status: "done"},
								{ID: "cmp_stack", Type: ServiceCompose, Name: "observability", Status: "running"},
								{ID: "pg_main", Type: ServicePostgres},
								{ID: "mysql_legacy", Type: ServiceMySQL},
								{ID: "mongo_sessions", Type: ServiceMongo},
								{ID: "redis_cache", Type: ServiceRedis},
								{ID: "libsql_edge", Type: ServiceLibSQL},
							},
						},
						{
							ID: "env_shop_stg", Name: "staging",
							Services: []Service{
								{ID: "maria_stg", Type: ServiceMariaDB},
							},
						},
					},
				},
				{ID: "prj_empty", Name: "empty", Environments: []Environment{}},
			},
		},
		{
			name:    "member sees only granted services with names",
			fixture: "project.all.member.json",
			want: []Project{
				{
					ID: "prj_shop", Name: "shop",
					Environments: []Environment{
						{
							ID: "env_shop_prod", Name: "production", IsDefault: true,
							Services: []Service{
								{ID: "app_web", Type: ServiceApplication, Name: "web", Status: "done"},
								{ID: "pg_main", Type: ServicePostgres, Name: "main-db", Status: "running"},
							},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fixturePanel(t, map[string]string{"project.all": tt.fixture})
			got, err := newClient(t, srv.URL, testKey).Projects(context.Background())
			if err != nil {
				t.Fatalf("Projects: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Projects =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestClient_ProjectsErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"rejected key", 401, `{"message":"Unauthorized"}`, ErrUnauthorized},
		{"forbidden", 403, `{"message":"Forbidden","code":"FORBIDDEN"}`, ErrUnauthorized},
		{"not a Dokploy panel", 200, `<html></html>`, ErrUnexpectedResponse},
		{"object instead of list", 200, `{"projects":[]}`, ErrUnexpectedResponse},
		{"project without ID", 200, `[{"projectId":"","name":"x","environments":[]}]`, ErrUnexpectedResponse},
		{"environment without ID", 200, `[{"projectId":"p","name":"x","environments":[{"environmentId":"","name":"e"}]}]`, ErrUnexpectedResponse},
		{"service without ID", 200, `[{"projectId":"p","name":"x","environments":[{"environmentId":"e","name":"e","redis":[{"redisId":""}]}]}]`, ErrUnexpectedResponse},
		{"server error", 500, `oops`, ErrUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, testKey).Projects(context.Background())
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClient_ProjectsNullIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `null`)
	}))
	defer srv.Close()
	got, err := newClient(t, srv.URL, testKey).Projects(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("Projects = %v, %v; want no projects and no error", got, err)
	}
}

func TestClient_ProjectsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	_, err := newClient(t, base, testKey).Projects(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}

func TestServiceTypes_CoverProjectAll(t *testing.T) {
	want := []ServiceType{
		ServiceApplication, ServiceCompose, ServicePostgres, ServiceMySQL,
		ServiceMariaDB, ServiceMongo, ServiceRedis, ServiceLibSQL,
	}
	if got := ServiceTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("ServiceTypes() = %v, want %v", got, want)
	}
}

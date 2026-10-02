package dokploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// detailRoutes serves every <type>.one fixture for the IDs project.all
// returns in project.all.owner.json.
var detailRoutes = map[string]string{
	"application.one?applicationId=app_web": "application.one.json",
	"compose.one?composeId=cmp_stack":       "compose.one.json",
	"postgres.one?postgresId=pg_main":       "postgres.one.json",
	"mysql.one?mysqlId=mysql_legacy":        "mysql.one.json",
	"mariadb.one?mariadbId=maria_stg":       "mariadb.one.json",
	"mongo.one?mongoId=mongo_sessions":      "mongo.one.json",
	"redis.one?redisId=redis_cache":         "redis.one.json",
	"libsql.one?libsqlId=libsql_edge":       "libsql.one.json",
}

func TestClient_Details(t *testing.T) {
	tests := []struct {
		typ  ServiceType
		id   string
		want ServiceDetails
	}{
		{
			// Applications listen wherever their image does: the port stays
			// unknown, and the published ports and domain targets are
			// reported as evidence.
			ServiceApplication, "app_web",
			ServiceDetails{
				Service: Service{ID: "app_web", Type: ServiceApplication, Name: "web", Status: "done"},
				AppName: "shop-web-a1b2c3",
				Ports:   []Port{{Target: 9090, Published: 9100, Protocol: "tcp"}},
				Domains: []Domain{{Host: "shop.example.com", Port: 8080}},
				Networks: Networks{
					NetworkIDs: []string{"net_backend"},
				},
			},
		},
		{
			ServiceCompose, "cmp_stack",
			ServiceDetails{
				Service:     Service{ID: "cmp_stack", Type: ServiceCompose, Name: "observability", Status: "running"},
				AppName:     "shop-observability-d4e5f6",
				ServerID:    "srv_edge",
				ComposeType: "docker-compose",
				Domains:     []Domain{{Host: "grafana.example.com", Port: 3000, ComposeService: "grafana"}},
				Networks: Networks{
					Isolated: true,
					ComposeServices: []ComposeServiceNetworks{
						{ServiceName: "prometheus", NetworkIDs: []string{"net_metrics"}, DetachDokployNetwork: true},
					},
				},
			},
		},
		{
			ServicePostgres, "pg_main",
			ServiceDetails{
				Service:     Service{ID: "pg_main", Type: ServicePostgres, Name: "main-db", Status: "running"},
				AppName:     "shop-maindb-111111",
				DefaultPort: 5432,
				Ports:       []Port{{Target: 5432, Published: 5433, Protocol: "tcp"}},
				Networks: Networks{
					NetworkIDs:           []string{"net_backend", "net_analytics"},
					DetachDokployNetwork: true,
				},
			},
		},
		{
			ServiceMySQL, "mysql_legacy",
			ServiceDetails{
				Service:     Service{ID: "mysql_legacy", Type: ServiceMySQL, Name: "legacy", Status: "done"},
				AppName:     "shop-legacy-222222",
				DefaultPort: 3306,
				Ports:       []Port{{Target: 3306, Protocol: "tcp"}},
			},
		},
		{
			ServiceMariaDB, "maria_stg",
			ServiceDetails{
				Service:     Service{ID: "maria_stg", Type: ServiceMariaDB, Name: "staging-db", Status: "idle"},
				AppName:     "shop-stagingdb-333333",
				DefaultPort: 3306,
				Ports:       []Port{{Target: 3306, Protocol: "tcp"}},
			},
		},
		{
			ServiceMongo, "mongo_sessions",
			ServiceDetails{
				Service:     Service{ID: "mongo_sessions", Type: ServiceMongo, Name: "sessions", Status: "running"},
				AppName:     "shop-sessions-444444",
				DefaultPort: 27017,
				Ports:       []Port{{Target: 27017, Protocol: "tcp"}},
				Networks: Networks{
					SwarmTargets: []string{"custom-overlay"},
					NetworkIDs:   []string{"net_ignored"},
				},
			},
		},
		{
			ServiceRedis, "redis_cache",
			ServiceDetails{
				Service:     Service{ID: "redis_cache", Type: ServiceRedis, Name: "cache", Status: "error"},
				AppName:     "shop-cache-555555",
				ServerID:    "srv_edge",
				DefaultPort: 6379,
				Ports:       []Port{{Target: 6379, Protocol: "tcp"}},
			},
		},
		{
			ServiceLibSQL, "libsql_edge",
			ServiceDetails{
				Service:     Service{ID: "libsql_edge", Type: ServiceLibSQL, Name: "edge-db", Status: "running"},
				AppName:     "shop-edgedb-666666",
				DefaultPort: 8080,
				Ports: []Port{
					{Name: "http", Target: 8080, Published: 8081, Protocol: "tcp"},
					{Name: "grpc", Target: 5001, Protocol: "tcp"},
					{Name: "admin", Target: 5000, Published: 5050, Protocol: "tcp"},
				},
			},
		},
	}
	srv := fixturePanel(t, detailRoutes)
	client := newClient(t, srv.URL, testKey)
	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			got, err := client.Details(context.Background(), tt.typ, tt.id)
			if err != nil {
				t.Fatalf("Details: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Details =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// Every type project.all lists must have a detail fixture, so a new type
// cannot be added without teaching Details about it.
func TestClient_DetailsCoverEveryServiceType(t *testing.T) {
	for _, typ := range ServiceTypes() {
		var found bool
		for route := range detailRoutes {
			found = found || strings.HasPrefix(route, string(typ)+".one?")
		}
		if !found {
			t.Errorf("no detail fixture for %s", typ)
		}
	}
}

func TestClient_DetailsErrors(t *testing.T) {
	const valid = `{"redisId":"r1","name":"cache","appName":"cache-1","applicationStatus":"done","serverId":null}`
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"no access to the service", 401, `{"message":"You don't have access to this service","code":"UNAUTHORIZED"}`, ErrUnauthorized},
		{"forbidden", 403, `{"message":"Forbidden","code":"FORBIDDEN"}`, ErrUnauthorized},
		{"deleted service", 404, `{"message":"Redis not found","code":"NOT_FOUND"}`, ErrNotFound},
		{"404 from something else", 404, `<html>Not Found</html>`, ErrUnexpectedResponse},
		{"not a Dokploy panel", 200, `<html></html>`, ErrUnexpectedResponse},
		{"another service returned", 200, strings.Replace(valid, `"r1"`, `"r2"`, 1), ErrUnexpectedResponse},
		{"no app name", 200, strings.Replace(valid, `"cache-1"`, `""`, 1), ErrUnexpectedResponse},
		{"external port out of range", 200, strings.Replace(valid, `"serverId":null`, `"externalPort":70000`, 1), ErrUnexpectedResponse},
		{"null", 200, `null`, ErrUnexpectedResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, testKey).Details(context.Background(), ServiceRedis, "r1")
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClient_DetailsRejectsInvalidPorts(t *testing.T) {
	tests := []struct {
		name string
		typ  ServiceType
		body string
	}{
		{"application port out of range", ServiceApplication,
			`{"applicationId":"a1","appName":"a","ports":[{"publishedPort":80,"targetPort":0,"protocol":"tcp"}]}`},
		{"domain port out of range", ServiceApplication,
			`{"applicationId":"a1","appName":"a","domains":[{"host":"x.example.com","port":65536}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			_, err := newClient(t, srv.URL, testKey).Details(context.Background(), tt.typ, "a1")
			if !errors.Is(err, ErrUnexpectedResponse) {
				t.Errorf("err = %v, want ErrUnexpectedResponse", err)
			}
		})
	}
}

func TestClient_DetailsRejectsBadArguments(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	client := newClient(t, srv.URL, testKey)
	if _, err := client.Details(context.Background(), ServiceType("kubernetes"), "x"); err == nil {
		t.Error("unknown service type: want an error")
	}
	if _, err := client.Details(context.Background(), ServicePostgres, ""); err == nil {
		t.Error("empty ID: want an error")
	}
	if called {
		t.Error("a request was sent for invalid arguments")
	}
}

func TestClient_DetailsWrongKey(t *testing.T) {
	srv := fixturePanel(t, detailRoutes)
	_, err := newClient(t, srv.URL, "wrong").Details(context.Background(), ServicePostgres, "pg_main")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestDefaultPort_PerType(t *testing.T) {
	want := map[ServiceType]int{
		ServiceApplication:   UnknownPort,
		ServiceCompose:       UnknownPort,
		ServicePostgres:      5432,
		ServiceMySQL:         3306,
		ServiceMariaDB:       3306,
		ServiceMongo:         27017,
		ServiceRedis:         6379,
		ServiceLibSQL:        8080,
		ServiceType("bogus"): UnknownPort,
	}
	for typ, port := range want {
		if got := DefaultPort(typ); got != port {
			t.Errorf("DefaultPort(%s) = %d, want %d", typ, got, port)
		}
	}
	for _, typ := range ServiceTypes() {
		if _, ok := want[typ]; !ok {
			t.Errorf("no expected default port for %s", typ)
		}
	}
}

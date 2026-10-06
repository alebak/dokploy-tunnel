package tunnel

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
)

func TestParseTarget_Valid(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  Target
	}{
		{
			"database",
			"serviceType=postgres&serviceId=pg_main&port=5432",
			Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432},
		},
		{
			"application",
			"serviceType=application&serviceId=app-web_1&port=8080",
			Target{ServiceType: dokploy.ServiceApplication, ServiceID: "app-web_1", Port: 8080},
		},
		{
			"compose stack",
			"serviceType=compose&serviceId=cmp_myapp&port=80",
			Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", Port: 80},
		},
		{
			"service inside a compose stack",
			"serviceType=compose_service&serviceId=cmp_myapp%2Fpostgres.v2&port=5432",
			Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres.v2", Port: 5432},
		},
		{
			"unknown parameters are ignored",
			"serviceType=redis&serviceId=r1&port=65535&extra=1",
			Target{ServiceType: dokploy.ServiceRedis, ServiceID: "r1", Port: 65535},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseTarget(q)
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseTarget(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

func TestParseTarget_Invalid(t *testing.T) {
	long := strings.Repeat("a", 129)
	tests := []struct {
		name  string
		query string
	}{
		{"missing type", "serviceId=pg_main&port=5432"},
		{"unknown type", "serviceType=docker&serviceId=pg_main&port=5432"},
		{"missing ID", "serviceType=postgres&port=5432"},
		{"ID with a slash", "serviceType=postgres&serviceId=pg%2Fmain&port=5432"},
		{"ID with a space", "serviceType=postgres&serviceId=pg+main&port=5432"},
		{"ID too long", "serviceType=postgres&serviceId=" + long + "&port=5432"},
		{"repeated ID", "serviceType=postgres&serviceId=a&serviceId=b&port=5432"},
		{"missing port", "serviceType=postgres&serviceId=pg_main"},
		{"port zero", "serviceType=postgres&serviceId=pg_main&port=0"},
		{"port too large", "serviceType=postgres&serviceId=pg_main&port=65536"},
		{"port not a number", "serviceType=postgres&serviceId=pg_main&port=pg"},
		{"port with a sign", "serviceType=postgres&serviceId=pg_main&port=%2B5432"},
		{"compose service without a name", "serviceType=compose_service&serviceId=cmp_myapp&port=5432"},
		{"compose service with an empty name", "serviceType=compose_service&serviceId=cmp_myapp%2F&port=5432"},
		{"compose service with two slashes", "serviceType=compose_service&serviceId=cmp%2Fa%2Fb&port=5432"},
		{"compose service with an invalid name", "serviceType=compose_service&serviceId=cmp%2Fa%24b&port=5432"},
		{"compose service name too long", "serviceType=compose_service&serviceId=cmp%2F" + long + "&port=5432"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseTarget(q)
			if !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("ParseTarget(%q) = %+v, %v; want ErrInvalidTarget", tt.query, got, err)
			}
		})
	}
}

func TestTarget_Query_RoundTrips(t *testing.T) {
	targets := []Target{
		{ServiceType: dokploy.ServiceMongo, ServiceID: "mongo_sessions", Port: 27017},
		{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432},
	}
	for _, want := range targets {
		got, err := ParseTarget(want.Query())
		if err != nil {
			t.Fatalf("ParseTarget(%v): %v", want.Query(), err)
		}
		if got != want {
			t.Errorf("round trip of %+v = %+v", want, got)
		}
	}
}

func TestTarget_String(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		{Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432}, "postgres pg_main:5432"},
		{Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432}, "compose_service cmp_myapp/postgres:5432"},
	}
	for _, tt := range tests {
		if got := tt.target.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestParseServerID(t *testing.T) {
	tests := []struct {
		query   string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"serverId=srv_edge", "srv_edge", false},
		{"serverId=local", LocalServer, false},
		{"serverId=", "", true},
		{"serverId=a%2Fb", "", true},
		{"serverId=a&serverId=b", "", true},
	}
	for _, tt := range tests {
		q, _ := url.ParseQuery(tt.query)
		got, err := ParseServerID(q)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseServerID(%q) = %q, %v; want %q, error %v", tt.query, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseTargetRef(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  TargetRef
	}{
		{
			"database",
			"serviceType=postgres&serviceId=pg_main",
			TargetRef{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main"},
		},
		{
			"service inside a compose stack on a server",
			"serviceType=compose_service&serviceId=cmp_myapp%2Fpostgres&serverId=srv_edge",
			TargetRef{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", ServerID: "srv_edge"},
		},
		{
			"a port is ignored",
			"serviceType=redis&serviceId=r1&port=6379",
			TargetRef{ServiceType: dokploy.ServiceRedis, ServiceID: "r1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseTargetRef(q)
			if err != nil {
				t.Fatalf("ParseTargetRef(%q): %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseTargetRef(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

func TestParseTargetRef_Invalid(t *testing.T) {
	for _, query := range []string{
		"serviceId=pg_main",
		"serviceType=postgres",
		"serviceType=nginx&serviceId=x",
		"serviceType=compose_service&serviceId=cmp_myapp",
		"serviceType=postgres&serviceId=a%2Fb",
		"serviceType=postgres&serviceId=pg_main&serverId=a%2Fb",
	} {
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTargetRef(q); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("ParseTargetRef(%q) error = %v, want ErrInvalidTarget", query, err)
		}
	}
}

func TestTargetRef_Query_RoundTrips(t *testing.T) {
	for _, ref := range []TargetRef{
		{ServiceType: dokploy.ServiceMySQL, ServiceID: "my_1"},
		{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_a", ComposeService: "db", ServerID: LocalServer},
	} {
		q := ref.Query()
		if q.Has(ParamPort) {
			t.Errorf("Query of %+v has a port: %v", ref, q)
		}
		got, err := ParseTargetRef(q)
		if err != nil || got != ref {
			t.Errorf("ParseTargetRef(%v) = %+v, %v; want %+v", q, got, err, ref)
		}
	}
}

func TestTargetRef_Target(t *testing.T) {
	ref := TargetRef{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_a", ComposeService: "db", ServerID: "srv"}
	want := Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_a", ComposeService: "db", Port: 5432}
	if got := ref.Target(5432); got != want {
		t.Errorf("Target(5432) = %+v, want %+v", got, want)
	}
}

func TestPortsResponse_JSON(t *testing.T) {
	raw, err := json.Marshal(PortsResponse{Ports: []Port{{Port: 5432, Protocol: ProtocolTCP}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ports":[{"port":5432,"protocol":"tcp"}]}`; string(raw) != want {
		t.Errorf("JSON = %s, want %s", raw, want)
	}
}

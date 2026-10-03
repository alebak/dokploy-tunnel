package tunnel

import (
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

package companion

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

func TestAuthorizer_Authorize_Allowed(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		target    tunnel.Target
		wantCalls []string
	}{
		{
			"owner reads a database",
			ownerKey,
			tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432},
			[]string{"postgres.one?postgresId=pg_main"},
		},
		{
			"member reads a granted application",
			memberKey,
			tunnel.Target{ServiceType: dokploy.ServiceApplication, ServiceID: "app_web", Port: 8080},
			[]string{"application.one?applicationId=app_web"},
		},
		{
			"owner reads a service inside a compose stack",
			ownerKey,
			tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "pgadmin", Port: 80},
			[]string{"compose.one?composeId=cmp_myapp", "compose.loadServices?composeId=cmp_myapp&type=cache"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDokploy(t)
			a := NewAuthorizer(fake.start(), "")

			got, err := a.Authorize(context.Background(), tt.key, tt.target, "")
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if got.Target != tt.target {
				t.Errorf("Target = %+v, want %+v", got.Target, tt.target)
			}
			if got.Service.ID != tt.target.ServiceID || got.Service.AppName == "" {
				t.Errorf("Service = %+v, want the details of %s", got.Service, tt.target.ServiceID)
			}
			if want := []string{"net_backend"}; !reflect.DeepEqual(got.Service.Networks.NetworkIDs, want) {
				t.Errorf("networks = %v, want %v", got.Service.Networks.NetworkIDs, want)
			}
			if calls := fake.recorded(); !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Errorf("Dokploy calls = %v, want %v", calls, tt.wantCalls)
			}
			for _, k := range fake.usedKeys() {
				if k != tt.key {
					t.Errorf("Dokploy was called with key %q, want the caller's", k)
				}
			}
		})
	}
}

func TestAuthorizer_Authorize_Denied(t *testing.T) {
	pg := tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432}
	tests := []struct {
		name   string
		key    string
		target tunnel.Target
		want   error
	}{
		{"invalid key", "dok_revoked", pg, ErrPermissionDenied},
		{"key of another organization", foreignKey, pg, ErrPermissionDenied},
		{"member without access", memberKey, pg, ErrPermissionDenied},
		{
			// Indistinguishable from a service the key cannot read.
			"service that does not exist", ownerKey,
			tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_gone", Port: 5432},
			ErrPermissionDenied,
		},
		{
			"service of another type", ownerKey,
			tunnel.Target{ServiceType: dokploy.ServiceRedis, ServiceID: "pg_main", Port: 6379},
			ErrPermissionDenied,
		},
		{
			"member without access to the compose stack", memberKey,
			tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432},
			ErrPermissionDenied,
		},
		{
			"compose service missing from the compose file", ownerKey,
			tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "redis", Port: 6379},
			ErrNotFound,
		},
		{
			"compose stack without a stored compose file", ownerKey,
			tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_new", ComposeService: "postgres", Port: 5432},
			ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDokploy(t)
			a := NewAuthorizer(fake.start(), "")

			got, err := a.Authorize(context.Background(), tt.key, tt.target, "")
			if !errors.Is(err, tt.want) {
				t.Fatalf("Authorize = %+v, %v; want %v", got, err, tt.want)
			}
			if strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q contains the API key", err)
			}
		})
	}
}

func TestAuthorizer_Authorize_WrongServer(t *testing.T) {
	tests := []struct {
		name       string
		companion  string
		serviceID  string
		wantServer string
		want       string
	}{
		{"remote service on the main companion", "", "pg_edge", "", "srv_edge"},
		{"main-server service on a remote companion", "srv_edge", "pg_main", "", tunnel.LocalServer},
		{"client expects another server", "", "pg_main", "srv_edge", tunnel.LocalServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDokploy(t)
			a := NewAuthorizer(fake.start(), tt.companion)
			target := tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: tt.serviceID, Port: 5432}

			_, err := a.Authorize(context.Background(), ownerKey, target, tt.wantServer)
			var wrong *WrongServerError
			if !errors.As(err, &wrong) {
				t.Fatalf("Authorize = %v, want a WrongServerError", err)
			}
			if wrong.Expected != tt.want {
				t.Errorf("Expected = %q, want %q", wrong.Expected, tt.want)
			}
		})
	}
}

func TestAuthorizer_Authorize_MatchingServer(t *testing.T) {
	tests := []struct {
		name       string
		companion  string
		serviceID  string
		wantServer string
	}{
		{"remote companion", "srv_edge", "pg_edge", "srv_edge"},
		{"main companion, server named local", "", "pg_main", tunnel.LocalServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDokploy(t)
			a := NewAuthorizer(fake.start(), tt.companion)
			target := tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: tt.serviceID, Port: 5432}

			if _, err := a.Authorize(context.Background(), ownerKey, target, tt.wantServer); err != nil {
				t.Fatalf("Authorize: %v", err)
			}
		})
	}
}

func TestAuthorizer_Authorize_WrongServerSkipsComposeFile(t *testing.T) {
	fake := newFakeDokploy(t)
	a := NewAuthorizer(fake.start(), "srv_edge")
	target := tunnel.Target{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_myapp", ComposeService: "postgres", Port: 5432}

	var wrong *WrongServerError
	if _, err := a.Authorize(context.Background(), ownerKey, target, ""); !errors.As(err, &wrong) {
		t.Fatalf("Authorize = %v, want a WrongServerError", err)
	}
	if calls := fake.recorded(); !reflect.DeepEqual(calls, []string{"compose.one?composeId=cmp_myapp"}) {
		t.Errorf("Dokploy calls = %v, want only compose.one", calls)
	}
}

func TestAuthorizer_Authorize_DokployFailures(t *testing.T) {
	pg := tunnel.Target{ServiceType: dokploy.ServicePostgres, ServiceID: "pg_main", Port: 5432}

	t.Run("broken panel", func(t *testing.T) {
		fake := newFakeDokploy(t)
		fake.broken = true
		_, err := NewAuthorizer(fake.start(), "").Authorize(context.Background(), ownerKey, pg, "")
		if !errors.Is(err, ErrDokployUnreachable) {
			t.Fatalf("Authorize = %v, want ErrDokployUnreachable", err)
		}
	})

	t.Run("slow panel", func(t *testing.T) {
		fake := newFakeDokploy(t)
		fake.block = make(chan struct{})
		t.Cleanup(func() { close(fake.block) })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := NewAuthorizer(fake.start(), "").Authorize(ctx, ownerKey, pg, "")
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("Authorize = %v, want ErrTimeout", err)
		}
	})
}

package forward

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
	"github.com/alebak/dokploy-tunnel/internal/dokploy"
	"github.com/alebak/dokploy-tunnel/internal/tunnel"
)

func newTestClient(t *testing.T, companionURL string) *Client {
	t.Helper()
	c, err := NewClient(companionURL, testKey)
	if err != nil {
		t.Fatalf("NewClient(%q) error = %v", companionURL, err)
	}
	return c
}

func TestNewClient_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, url, key string
	}{
		{"no scheme", "dokploy.example.com/doktunnel", testKey},
		{"unsupported scheme", "ftp://dokploy.example.com", testKey},
		{"no host", "https:///doktunnel", testKey},
		{"query", "https://dokploy.example.com/doktunnel?x=1", testKey},
		{"user info", "https://user:pass@dokploy.example.com", testKey},
		{"empty key", "https://dokploy.example.com", ""},
		{"oversized key", "https://dokploy.example.com", strings.Repeat("k", tunnel.MaxAPIKeyBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewClient(tt.url, tt.key); err == nil {
				t.Errorf("NewClient(%q) error = nil, want an error", tt.url)
			}
		})
	}
}

func TestClient_Ports_Success(t *testing.T) {
	fc := newFakeCompanion(t)
	fc.ports = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"ports":[{"port":15672,"protocol":"tcp"},{"port":5672,"protocol":"tcp"}]}`)
	}
	// A trailing slash on the companion URL must not double the slash.
	c := newTestClient(t, fc.url()+"/")
	ref := tunnel.TargetRef{ServiceType: dokploy.ServiceCompose, ServiceID: "cmp_app", ComposeService: "rabbit", ServerID: "srv_edge"}

	got, err := c.Ports(context.Background(), ref)
	if err != nil {
		t.Fatalf("Ports() error = %v", err)
	}
	want := []tunnel.Port{{Port: 5672, Protocol: "tcp"}, {Port: 15672, Protocol: "tcp"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ports() = %v, want %v (sorted by port)", got, want)
	}

	r := fc.lastRequest()
	if !hasPrefixPath(r, tunnel.PortsPath) {
		t.Errorf("request path = %q, want %q", r.URL.Path, testPrefix+tunnel.PortsPath)
	}
	if r.Header.Get(tunnel.HeaderAPIKey) != testKey {
		t.Errorf("x-api-key = %q, want the API key", r.Header.Get(tunnel.HeaderAPIKey))
	}
	q := queryOf(r)
	for param, want := range map[string]string{
		tunnel.ParamServiceType: tunnel.TypeComposeService,
		tunnel.ParamServiceID:   "cmp_app/rabbit",
		tunnel.ParamServerID:    "srv_edge",
	} {
		if got := q.Get(param); got != want {
			t.Errorf("query %s = %q, want %q", param, got, want)
		}
	}
	if q.Has(tunnel.ParamPort) {
		t.Errorf("query has %s = %q, want no port on a ports request", tunnel.ParamPort, q.Get(tunnel.ParamPort))
	}
}

func TestClient_Ports_EmptyListIsNotNil(t *testing.T) {
	fc := newFakeCompanion(t)
	fc.ports = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"ports":[]}`)
	}
	got, err := newTestClient(t, fc.url()).Ports(context.Background(), pgRef)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Ports() = %#v, %v; want an empty non-nil list", got, err)
	}
	if fc.lastRequest().URL.Query().Has(tunnel.ParamServerID) {
		t.Error("query has serverId, want none when no server is expected")
	}
}

func TestClient_Ports_Errors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   tunnel.Code
		wantCLI    clierr.Code
		wantInText string
	}{
		{
			name: "permission denied", status: http.StatusForbidden,
			body:     `{"code":"permission_denied","message":"the API key cannot read this service"}`,
			wantCode: tunnel.CodePermissionDenied, wantCLI: clierr.PermissionDenied,
			wantInText: "cannot read this service",
		},
		{
			name: "wrong server", status: http.StatusMisdirectedRequest,
			body:     `{"code":"wrong_server","message":"elsewhere","expected_server_id":"srv_edge"}`,
			wantCode: tunnel.CodeWrongServer, wantCLI: clierr.Unreachable, wantInText: "srv_edge",
		},
		{
			name: "proxy page", status: http.StatusBadGateway, body: `<html>bad gateway</html>`,
			wantCode: "", wantCLI: clierr.Unreachable, wantInText: "HTTP 502",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeCompanion(t)
			fc.ports = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, tt.status, tt.body) }

			_, err := newTestClient(t, fc.url()).Ports(context.Background(), pgRef)
			var ce *CompanionError
			if !errors.As(err, &ce) {
				t.Fatalf("Ports() error = %v, want a *CompanionError", err)
			}
			if ce.Code != tt.wantCode || ce.Status != tt.status {
				t.Errorf("error code, status = %q, %d; want %q, %d", ce.Code, ce.Status, tt.wantCode, tt.status)
			}
			cli := CLIError(err)
			if cli.Code != tt.wantCLI {
				t.Errorf("CLIError().Code = %q, want %q", cli.Code, tt.wantCLI)
			}
			if !strings.Contains(cli.Message+" "+cli.Hint, tt.wantInText) {
				t.Errorf("CLIError() = %q (hint %q), want it to mention %q", cli.Message, cli.Hint, tt.wantInText)
			}
		})
	}
}

func TestClient_Ports_RejectsInvalidAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"port out of range": `{"ports":[{"port":70000,"protocol":"tcp"}]}`,
		"udp port":          `{"ports":[{"port":53,"protocol":"udp"}]}`,
		"not json":          `ports: 5432`,
	} {
		t.Run(name, func(t *testing.T) {
			fc := newFakeCompanion(t)
			fc.ports = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, body) }
			_, err := newTestClient(t, fc.url()).Ports(context.Background(), pgRef)
			if !errors.Is(err, ErrCompanionUnreachable) {
				t.Errorf("Ports() error = %v, want ErrCompanionUnreachable", err)
			}
		})
	}
}

func TestClient_Ports_UnreachableCompanion(t *testing.T) {
	fc := newFakeCompanion(t)
	u := fc.url()
	fc.srv.Close()
	_, err := newTestClient(t, u).Ports(context.Background(), pgRef)
	if !errors.Is(err, ErrCompanionUnreachable) {
		t.Fatalf("Ports() error = %v, want ErrCompanionUnreachable", err)
	}
	if got := CLIError(err).Code; got != clierr.Unreachable {
		t.Errorf("CLIError().Code = %q, want %q", got, clierr.Unreachable)
	}
}

func TestClient_Ports_DoesNotFollowRedirects(t *testing.T) {
	fc := newFakeCompanion(t)
	fc.ports = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/steal", http.StatusFound)
	}
	_, err := newTestClient(t, fc.url()).Ports(context.Background(), pgRef)
	var ce *CompanionError
	if !errors.As(err, &ce) || ce.Status != http.StatusFound {
		t.Errorf("Ports() error = %v, want a *CompanionError with HTTP 302", err)
	}
}

func TestCLIError_MapsCompanionCodes(t *testing.T) {
	tests := []struct {
		code tunnel.Code
		want clierr.Code
	}{
		{tunnel.CodeUnauthenticated, clierr.PermissionDenied},
		{tunnel.CodePermissionDenied, clierr.PermissionDenied},
		{tunnel.CodeForbiddenOrigin, clierr.PermissionDenied},
		{tunnel.CodeNotFound, clierr.NotFound},
		{tunnel.CodeNetworkNotAttachable, clierr.NetworkNotAttachable},
		{tunnel.CodeWrongServer, clierr.Unreachable},
		{codeTooManyTunnels, clierr.Unreachable},
		{tunnel.CodeUnreachable, clierr.Unreachable},
		{tunnel.CodeTargetUnreachable, clierr.Unreachable},
		{tunnel.CodeTimeout, clierr.Unreachable},
		{tunnel.CodeUnavailable, clierr.Unreachable},
		{tunnel.CodeInvalidArgument, clierr.InvalidArgument},
		{tunnel.CodeUpgradeRequired, clierr.Internal},
		{tunnel.CodeInternal, clierr.Internal},
		{"some_future_code", clierr.Internal},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			err := &CompanionError{Status: 400, Code: tt.code, Message: "m"}
			if got := CLIError(err).Code; got != tt.want {
				t.Errorf("CLIError(%q).Code = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestCLIError_PassesThroughOtherErrors(t *testing.T) {
	if CLIError(nil) != nil {
		t.Error("CLIError(nil) != nil")
	}
	typed := clierr.New(clierr.MissingInput, "x")
	if got := CLIError(typed); got != typed {
		t.Errorf("CLIError(typed) = %v, want it unchanged", got)
	}
	if got := CLIError(errors.New("boom")).Code; got != clierr.Internal {
		t.Errorf("CLIError(untyped).Code = %q, want %q", got, clierr.Internal)
	}
}

package dokploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamingPanel answers procedure with a body of exactly size bytes,
// written in chunks so the test never holds the whole body in memory.
func streamingPanel(t *testing.T, procedure string, size int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/"+procedure {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not found","code":"NOT_FOUND"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for left := size; left > 0; {
			n := min(left, int64(len(chunk)))
			if _, err := w.Write(chunk[:n]); err != nil {
				return // the client stopped reading
			}
			left -= n
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_ResponseTooLarge(t *testing.T) {
	tests := []struct {
		name      string
		procedure string
		limit     int64
		call      func(context.Context, *Client) error
	}{
		{
			name:      "small call keeps the default limit",
			procedure: "user.session",
			limit:     maxResponseBytes,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Organization(ctx)
				return err
			},
		},
		{
			name:      "project.all",
			procedure: "project.all",
			limit:     maxLargeResponseBytes,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Projects(ctx)
				return err
			},
		},
		{
			name:      "application.one",
			procedure: "application.one",
			limit:     maxLargeResponseBytes,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Details(ctx, ServiceApplication, "app_web")
				return err
			},
		},
		{
			name:      "compose.one",
			procedure: "compose.one",
			limit:     maxLargeResponseBytes,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Details(ctx, ServiceCompose, "cmp_stack")
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := streamingPanel(t, tt.procedure, tt.limit+1)
			err := tt.call(context.Background(), newClient(t, srv.URL, testKey))

			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("err = %v, want ErrResponseTooLarge", err)
			}
			var tooLarge *ResponseTooLargeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("err = %v, want a *ResponseTooLargeError", err)
			}
			if tooLarge.Procedure != tt.procedure || tooLarge.Limit != tt.limit {
				t.Errorf("error names %s with limit %d, want %s with limit %d",
					tooLarge.Procedure, tooLarge.Limit, tt.procedure, tt.limit)
			}
			for _, want := range []string{tt.procedure, fmt.Sprint(tt.limit)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			// Callers that only know the generic error keep working.
			if !errors.Is(err, ErrUnexpectedResponse) {
				t.Errorf("err = %v, want it to match ErrUnexpectedResponse too", err)
			}
		})
	}
}

func TestClient_ResponseAtLimitIsRead(t *testing.T) {
	// Exactly limit bytes is allowed: the error must come from decoding the
	// filler body, not from the size check.
	srv := streamingPanel(t, "user.session", maxResponseBytes)
	_, err := newClient(t, srv.URL, testKey).Organization(context.Background())
	if errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrUnexpectedResponse) {
		t.Errorf("err = %v, want a JSON decoding error", err)
	}
}

func TestClient_ProjectsAllowsLargeResponse(t *testing.T) {
	// A large organization: well over the default limit, under the
	// project.all limit.
	description := strings.Repeat("x", 4*maxResponseBytes)
	body := `[{"projectId":"prj_big","name":"big","description":"` + description + `","environments":[]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/project.all" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	projects, err := newClient(t, srv.URL, testKey).Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "prj_big" {
		t.Errorf("projects = %+v, want prj_big", projects)
	}
}

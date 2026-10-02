package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

func TestWriteError_JSONGoesToStdout(t *testing.T) {
	tests := []struct {
		name string
		err  *clierr.Error
		want map[string]any
	}{
		{
			name: "with hint",
			err:  clierr.New(clierr.MissingInput, "missing value for --context").WithHint("pass --context <name>"),
			want: map[string]any{"code": "missing_input", "message": "missing value for --context", "hint": "pass --context <name>"},
		},
		{
			name: "without hint omits the field",
			err:  clierr.New(clierr.NotImplemented, "services is not implemented yet"),
			want: map[string]any{"code": "not_implemented", "message": "services is not implemented yet"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			WriteError(&stdout, &stderr, true, tt.err)

			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
			var got map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v (%q)", err, stdout.String())
			}
			if hint, _ := tt.want["hint"].(string); !strings.Contains(stdout.String(), hint) {
				t.Errorf("stdout %q does not contain the hint verbatim", stdout.String())
			}
			if len(got) != len(tt.want) {
				t.Errorf("fields = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("field %q = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestWriteError_HumanGoesToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := clierr.New(clierr.MissingInput, "missing value for --context").WithHint("pass --context <name>")
	WriteError(&stdout, &stderr, false, err)

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	got := stderr.String()
	for _, want := range []string{"missing value for --context", "missing_input", "pass --context <name>"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr %q does not contain %q", got, want)
		}
	}
}

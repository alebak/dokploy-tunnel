package prompt

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

func TestNew_DisallowedReturnsMissingInputNamingFlag(t *testing.T) {
	in := New(false, strings.NewReader("ignored\n"), &bytes.Buffer{})

	_, err := in.Ask(Request{Flag: "context", Label: "Context"})

	var e *clierr.Error
	if !errors.As(err, &e) {
		t.Fatalf("Ask() error = %v, want *clierr.Error", err)
	}
	if e.Code != clierr.MissingInput {
		t.Errorf("code = %q, want %q", e.Code, clierr.MissingInput)
	}
	if !strings.Contains(e.Message+e.Hint, "--context") {
		t.Errorf("error %+v does not name --context", e)
	}
}

func TestNew_AllowedReadsAnswers(t *testing.T) {
	tests := []struct {
		name     string
		stdin    string
		want     []string
		wantCode clierr.Code
	}{
		{name: "single answer", stdin: "prod\n", want: []string{"prod"}},
		{name: "answers are trimmed", stdin: "  prod  \r\n", want: []string{"prod"}},
		{name: "consecutive answers", stdin: "prod\nstaging\n", want: []string{"prod", "staging"}},
		{name: "last line without newline", stdin: "prod", want: []string{"prod"}},
		{name: "empty answer is missing input", stdin: "\n", wantCode: clierr.MissingInput},
		{name: "closed stdin is missing input", stdin: "", wantCode: clierr.MissingInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prompts bytes.Buffer
			in := New(true, strings.NewReader(tt.stdin), &prompts)
			req := Request{Flag: "context", Label: "Context"}

			if tt.wantCode != "" {
				_, err := in.Ask(req)
				if got := clierr.From(err); got == nil || got.Code != tt.wantCode {
					t.Fatalf("Ask() error = %v, want code %q", err, tt.wantCode)
				}
				return
			}
			for _, want := range tt.want {
				got, err := in.Ask(req)
				if err != nil {
					t.Fatalf("Ask() error = %v", err)
				}
				if got != want {
					t.Errorf("Ask() = %q, want %q", got, want)
				}
			}
			if !strings.Contains(prompts.String(), "Context") {
				t.Errorf("prompt output %q does not show the label", prompts.String())
			}
		})
	}
}

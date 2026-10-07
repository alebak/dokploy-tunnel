package prompt

import (
	"bytes"
	"errors"
	"slices"
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

func testChoice() Choice {
	return Choice{
		Label:   "Services to forward",
		Options: []string{"main-db", "myapp/postgres", "cache"},
		Missing: clierr.New(clierr.MissingInput, "missing value for <service>").WithHint("pass <service> or --all"),
	}
}

func TestNew_DisallowedChooseReturnsTheChoiceMissingError(t *testing.T) {
	in := New(false, strings.NewReader("1\n"), &bytes.Buffer{})

	_, err := in.Choose(testChoice())

	e := clierr.From(err)
	if e.Code != clierr.MissingInput || !strings.Contains(e.Message+e.Hint, "--all") {
		t.Errorf("Choose() error = %+v, want the choice's missing_input error naming --all", e)
	}
}

func TestNew_AllowedChoose(t *testing.T) {
	tests := []struct {
		name     string
		stdin    string
		want     []int
		wantCode clierr.Code
	}{
		{name: "one number", stdin: "2\n", want: []int{1}},
		{name: "commas and spaces", stdin: " 3, 1 \n", want: []int{0, 2}},
		{name: "range", stdin: "1-2\n", want: []int{0, 1}},
		{name: "duplicates collapse", stdin: "2 2 1-2\n", want: []int{0, 1}},
		{name: "all", stdin: "all\n", want: []int{0, 1, 2}},
		{name: "empty answer is missing input", stdin: "\n", wantCode: clierr.MissingInput},
		{name: "closed stdin is missing input", stdin: "", wantCode: clierr.MissingInput},
		{name: "out of range", stdin: "4\n", wantCode: clierr.InvalidArgument},
		{name: "zero", stdin: "0\n", wantCode: clierr.InvalidArgument},
		{name: "not a number", stdin: "db\n", wantCode: clierr.InvalidArgument},
		{name: "reversed range", stdin: "3-1\n", wantCode: clierr.InvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prompts bytes.Buffer
			in := New(true, strings.NewReader(tt.stdin), &prompts)

			got, err := in.Choose(testChoice())
			if tt.wantCode != "" {
				if e := clierr.From(err); e == nil || e.Code != tt.wantCode {
					t.Fatalf("Choose() = %v, %v; want code %q", got, err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Choose() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Choose() = %v, want %v", got, tt.want)
			}
			for _, want := range []string{"Services to forward", "1) main-db", "2) myapp/postgres", "3) cache"} {
				if !strings.Contains(prompts.String(), want) {
					t.Errorf("prompt output %q does not show %q", prompts.String(), want)
				}
			}
		})
	}
}

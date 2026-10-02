package clierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCode_DistinctPerCode(t *testing.T) {
	seen := map[int]Code{}
	for _, code := range Codes() {
		exit := code.ExitCode()
		if exit == 0 {
			t.Errorf("code %q maps to exit code 0", code)
		}
		if other, ok := seen[exit]; ok {
			t.Errorf("codes %q and %q share exit code %d", other, code, exit)
		}
		seen[exit] = code
	}
}

func TestCodes_IncludesRequiredCodes(t *testing.T) {
	required := []Code{
		NotImplemented, InvalidArgument, MissingInput, PermissionDenied,
		NetworkNotAttachable, ElevationRequired, Unreachable, Internal,
	}
	known := map[Code]bool{}
	for _, c := range Codes() {
		known[c] = true
	}
	for _, c := range required {
		if !known[c] {
			t.Errorf("Codes() is missing %q", c)
		}
	}
}

func TestExitCode_UnknownCodeIsInternal(t *testing.T) {
	if got, want := Code("bogus").ExitCode(), Internal.ExitCode(); got != want {
		t.Errorf("ExitCode(bogus) = %d, want %d", got, want)
	}
}

func TestFrom_Scenarios(t *testing.T) {
	typed := New(MissingInput, "missing value for --context").WithHint("pass --context <name>")
	tests := []struct {
		name     string
		err      error
		wantCode Code
		wantMsg  string
		wantHint string
	}{
		{"typed error", typed, MissingInput, "missing value for --context", "pass --context <name>"},
		{"wrapped typed error", fmt.Errorf("resolving context: %w", typed), MissingInput, "missing value for --context", "pass --context <name>"},
		{"plain error becomes internal", errors.New("boom"), Internal, "boom", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := From(tt.err)
			if got.Code != tt.wantCode || got.Message != tt.wantMsg || got.Hint != tt.wantHint {
				t.Errorf("From() = %+v, want code=%q message=%q hint=%q", got, tt.wantCode, tt.wantMsg, tt.wantHint)
			}
		})
	}
}

func TestFrom_NilIsNil(t *testing.T) {
	if got := From(nil); got != nil {
		t.Errorf("From(nil) = %+v, want nil", got)
	}
}

func TestError_ErrorIncludesMessage(t *testing.T) {
	err := Newf(InvalidArgument, "unknown command %q", "nope")
	if got, want := err.Error(), `unknown command "nope"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

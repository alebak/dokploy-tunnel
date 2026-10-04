package platformtest

import "testing"

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "nothing set", env: nil, want: false},
		{name: "opt-in outside GitHub Actions", env: map[string]string{EnvVar: "1"}, want: false},
		{name: "GitHub Actions without opt-in", env: map[string]string{"GITHUB_ACTIONS": "true"}, want: false},
		{name: "opt-in with another value", env: map[string]string{EnvVar: "true", "GITHUB_ACTIONS": "true"}, want: false},
		{name: "opt-in on GitHub Actions", env: map[string]string{EnvVar: "1", "GITHUB_ACTIONS": "true"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			if got := Enabled(getenv); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

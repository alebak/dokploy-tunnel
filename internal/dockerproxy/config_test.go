package dockerproxy

import (
	"errors"
	"flag"
	"io"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/repeater"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			want: Config{Listen: ":2375", DockerHost: docker.DefaultHost, RepeaterImage: repeater.DefaultImage},
		},
		{
			name: "environment",
			env:  map[string]string{EnvListen: ":9000", EnvDockerHost: "unix:///run/docker.sock", EnvRepeaterImage: "alpine/socat:1"},
			want: Config{Listen: ":9000", DockerHost: "unix:///run/docker.sock", RepeaterImage: "alpine/socat:1"},
		},
		{
			name: "flags win",
			args: []string{"--listen", "127.0.0.1:2375", "--docker-host", "tcp://docker:2375", "--repeater-image", "socat:2"},
			env:  map[string]string{EnvListen: ":9000", EnvDockerHost: "unix:///run/docker.sock", EnvRepeaterImage: "alpine/socat:1"},
			want: Config{Listen: "127.0.0.1:2375", DockerHost: "tcp://docker:2375", RepeaterImage: "socat:2"},
		},
		{
			name: "version needs nothing else",
			args: []string{"--version", "--docker-host", "ftp://x"},
			want: Config{Listen: ":2375", DockerHost: "ftp://x", RepeaterImage: repeater.DefaultImage, Version: true},
		},
		{name: "unsupported Docker host", args: []string{"--docker-host", "ssh://host"}, wantErr: true},
		{name: "TCP host without a port", args: []string{"--docker-host", "tcp://docker"}, wantErr: true},
		{name: "unexpected argument", args: []string{"serve"}, wantErr: true},
		{name: "unknown flag", args: []string{"--privileged"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig(tt.args, func(k string) string { return tt.env[k] }, io.Discard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseConfig error = %v, want error %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseConfig = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseConfig_Help(t *testing.T) {
	if _, err := ParseConfig([]string{"--help"}, func(string) string { return "" }, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help = %v, want flag.ErrHelp", err)
	}
}

func TestNew_RefusesAnEmptyImage(t *testing.T) {
	if _, err := New(docker.DefaultHost, "", nil); err == nil {
		t.Error("New accepted an empty repeater image")
	}
}

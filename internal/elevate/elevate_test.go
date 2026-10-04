package elevate

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// capture returns an Exec that records the command instead of running it.
func capture(got **exec.Cmd, err error) func(*exec.Cmd) error {
	return func(c *exec.Cmd) error {
		*got = c
		return err
	}
}

func TestSudo_Run(t *testing.T) {
	var cmd *exec.Cmd
	s := Sudo{Exec: capture(&cmd, nil)}
	argv := []string{"/usr/local/bin/doktunnel", "hosts", "privileged-apply", "--entries-file", "/home/me/state/pending hosts"}
	if err := s.Run(context.Background(), argv); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := append([]string{"sudo", "--"}, argv...)
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("command = %q, want %q", cmd.Args, want)
	}
}

func TestSudo_RunFailure(t *testing.T) {
	var cmd *exec.Cmd
	s := Sudo{Exec: capture(&cmd, errors.New("exit status 1"))}
	if err := s.Run(context.Background(), []string{"/bin/doktunnel"}); err == nil {
		t.Fatal("Run succeeded although sudo failed")
	}
}

func TestSudo_Command(t *testing.T) {
	got := Sudo{}.Command([]string{"/opt/my tools/doktunnel", "hosts", "privileged-apply", "--entries-file", "/home/o'neil/pending"})
	want := `sudo '/opt/my tools/doktunnel' hosts privileged-apply --entries-file '/home/o'\''neil/pending'`
	if got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
}

func TestUAC_Run(t *testing.T) {
	var cmd *exec.Cmd
	u := UAC{Exec: capture(&cmd, nil)}
	argv := []string{`C:\Program Files\doktunnel\doktunnel.exe`, "hosts", "privileged-apply", "--entries-file", `C:\Users\me\AppData\Local\doktunnel\pending-hosts`}
	if err := u.Run(context.Background(), argv); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cmd.Args) != 5 || cmd.Args[0] != "powershell.exe" || cmd.Args[1] != "-NoProfile" ||
		cmd.Args[2] != "-NonInteractive" || cmd.Args[3] != "-Command" {
		t.Fatalf("command = %q, want powershell.exe -NoProfile -NonInteractive -Command <script>", cmd.Args)
	}
	script := cmd.Args[4]
	for _, part := range []string{
		`Start-Process -FilePath 'C:\Program Files\doktunnel\doktunnel.exe'`,
		`-ArgumentList 'hosts privileged-apply --entries-file C:\Users\me\AppData\Local\doktunnel\pending-hosts'`,
		"-Verb RunAs", "-Wait", "-PassThru", "exit $p.ExitCode",
	} {
		if !strings.Contains(script, part) {
			t.Errorf("script %q does not contain %q", script, part)
		}
	}
}

func TestUAC_Command(t *testing.T) {
	got := UAC{}.Command([]string{`C:\it's\doktunnel.exe`, "hosts", "privileged-apply", "--entries-file", `C:\a b\pending`})
	want := `Start-Process -Verb RunAs -Wait -FilePath 'C:\it''s\doktunnel.exe' -ArgumentList 'hosts privileged-apply --entries-file "C:\a b\pending"'`
	if got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
}

func TestWindowsArg(t *testing.T) {
	tests := map[string]string{
		"plain":             "plain",
		"":                  `""`,
		"with space":        `"with space"`,
		`C:\dir\`:           `C:\dir\`,
		`C:\a b\`:           `"C:\a b\\"`,
		`say "hi"`:          `"say \"hi\""`,
		`back\"slash quote`: `"back\\\"slash quote"`,
	}
	for in, want := range tests {
		if got := windowsArg(in); got != want {
			t.Errorf("windowsArg(%q) = %s, want %s", in, got, want)
		}
	}
}

package elevate

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
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
	argv := []string{"/usr/local/bin/doktunnel", "hosts", "privileged-apply", "--entries-file", "-"}
	entries := strings.NewReader("127.77.0.1\tdb.p.o.c.internal\n")
	if err := s.Run(context.Background(), argv, entries); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := append([]string{"sudo", "--"}, argv...)
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("command = %q, want %q", cmd.Args, want)
	}
	if cmd.Stdin != entries {
		t.Errorf("stdin = %v, want the entries reader", cmd.Stdin)
	}
	if !(Sudo{}).PipesStdin() {
		t.Error("Sudo does not pipe stdin")
	}
}

func TestSudo_RunWithoutStdinKeepsTheTerminal(t *testing.T) {
	var cmd *exec.Cmd
	if err := (Sudo{Exec: capture(&cmd, nil)}).Run(context.Background(), []string{"/bin/doktunnel"}, nil); err != nil {
		t.Fatal(err)
	}
	if cmd.Stdin != os.Stdin {
		t.Errorf("stdin = %v, want os.Stdin", cmd.Stdin)
	}
}

func TestSudo_RunFailure(t *testing.T) {
	var cmd *exec.Cmd
	s := Sudo{Exec: capture(&cmd, errors.New("exit status 1"))}
	if err := s.Run(context.Background(), []string{"/bin/doktunnel"}, nil); err == nil {
		t.Fatal("Run succeeded although sudo failed")
	}
}

func TestSudo_Command(t *testing.T) {
	got := Sudo{}.Command([]string{"/opt/my tools/doktunnel", "hosts", "privileged-apply", "--entries-file", "-"}, "/home/o'neil/pending")
	want := `sudo '/opt/my tools/doktunnel' hosts privileged-apply --entries-file - < '/home/o'\''neil/pending'`
	if got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
	if got, want := (Sudo{}).Command([]string{"/bin/doktunnel", "--clean"}, ""), "sudo /bin/doktunnel --clean"; got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
}

// decodeEncodedCommand returns the script of a powershell.exe
// -EncodedCommand argument: base64 of UTF-16LE.
func decodeEncodedCommand(t *testing.T, arg string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(arg)
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("EncodedCommand %q is not base64 UTF-16: %v", arg, err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestUAC_Run(t *testing.T) {
	var cmd *exec.Cmd
	u := UAC{Exec: capture(&cmd, nil)}
	argv := []string{`C:\Users\O’Brien\my tools\doktunnel.exe`, "hosts", "privileged-apply", "--entries-file", `C:\Users\O’Brien\AppData\Local\doktunnel\pending-hosts`}
	if err := u.Run(context.Background(), argv, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cmd.Args) != 5 || cmd.Args[0] != "powershell.exe" || cmd.Args[1] != "-NoProfile" ||
		cmd.Args[2] != "-NonInteractive" || cmd.Args[3] != "-EncodedCommand" {
		t.Fatalf("command = %q, want powershell.exe -NoProfile -NonInteractive -EncodedCommand <script>", cmd.Args)
	}
	script := decodeEncodedCommand(t, cmd.Args[4])
	for _, part := range []string{
		`Start-Process -FilePath 'C:\Users\O’’Brien\my tools\doktunnel.exe'`,
		`-ArgumentList 'hosts privileged-apply --entries-file C:\Users\O’’Brien\AppData\Local\doktunnel\pending-hosts'`,
		"-Verb RunAs", "-Wait", "-PassThru", "exit $p.ExitCode",
	} {
		if !strings.Contains(script, part) {
			t.Errorf("script %q does not contain %q", script, part)
		}
	}
	if (UAC{}).PipesStdin() {
		t.Error("UAC claims to pipe stdin")
	}
}

func TestUAC_RunRefusesStdin(t *testing.T) {
	var cmd *exec.Cmd
	err := UAC{Exec: capture(&cmd, nil)}.Run(context.Background(), []string{`C:\doktunnel.exe`}, strings.NewReader("x"))
	if err == nil || cmd != nil {
		t.Errorf("Run with stdin = %v (ran %v), want an error before running anything", err, cmd)
	}
}

func TestUAC_Command(t *testing.T) {
	got := UAC{}.Command([]string{`C:\it's\doktunnel.exe`, "hosts", "privileged-apply", "--entries-file", `C:\a b\pending`}, "")
	want := `Start-Process -Verb RunAs -Wait -FilePath 'C:\it''s\doktunnel.exe' -ArgumentList 'hosts privileged-apply --entries-file "C:\a b\pending"'`
	if got != want {
		t.Errorf("Command = %s, want %s", got, want)
	}
}

// PowerShell ends a single-quoted string at any of five quote characters,
// so each must be doubled.
func TestPSQuote(t *testing.T) {
	tests := map[string]string{
		`C:\plain`:     `'C:\plain'`,
		`it's`:         `'it''s'`,
		"O\u2018Brien": "'O\u2018\u2018Brien'",
		"O\u2019Brien": "'O\u2019\u2019Brien'",
		"O\u201aBrien": "'O\u201a\u201aBrien'",
		"O\u201bBrien": "'O\u201b\u201bBrien'",
	}
	for in, want := range tests {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %s, want %s", in, got, want)
		}
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

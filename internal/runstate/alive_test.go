package runstate

import (
	"os"
	"os/exec"
	"testing"
)

func TestAlive_CurrentProcess(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Errorf("Alive(%d) = false for the running test process", os.Getpid())
	}
}

func TestAlive_ExitedProcess(t *testing.T) {
	// The test binary exits at once when no test matches.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a child process: %v", err)
	}
	if pid := cmd.Process.Pid; Alive(pid) {
		t.Errorf("Alive(%d) = true for a process that exited and was waited for", pid)
	}
}

func TestAlive_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if Alive(pid) {
			t.Errorf("Alive(%d) = true, want false", pid)
		}
	}
}

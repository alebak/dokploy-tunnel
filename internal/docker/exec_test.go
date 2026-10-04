package docker_test

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/alebak/dokploy-tunnel/internal/docker"
	"github.com/alebak/dokploy-tunnel/internal/docker/dockertest"
)

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestExec_StreamsBothWaysWithHalfClose(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "rep1", Name: "repeater", Running: true})
	var gotCmd []string
	fake.ExecHandler = func(_ string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		gotCmd = cmd
		io.WriteString(stderr, "ready\n")
		// Upper-case stdin until it ends, then say goodbye: the reply after
		// EOF proves the client half-closed instead of closing.
		data, _ := io.ReadAll(stdin)
		stdout.Write(bytes.ToUpper(data))
		io.WriteString(stderr, "stdin closed\n")
		io.WriteString(stdout, " bye")
		return 0
	}
	c := newClient(t, fake)

	var stderr syncBuffer
	ex, err := c.Exec(testContext(t), "rep1", []string{"cat", "-"}, &stderr)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer ex.Close()
	if _, err := io.WriteString(ex, "hello repeater"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ex.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	out, err := io.ReadAll(ex)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(out), "HELLO REPEATER bye"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "ready\nstdin closed\n" {
		t.Errorf("stderr = %q", got)
	}
	if strings.Join(gotCmd, " ") != "cat -" {
		t.Errorf("cmd = %q", gotCmd)
	}
	exec := fake.Execs()
	if len(exec) != 1 || !exec[0].AttachStdin || !exec[0].AttachStdout || !exec[0].AttachStderr || exec[0].Tty {
		t.Errorf("exec config = %+v, want stdin, stdout and stderr attached without a TTY", exec)
	}
}

func TestExec_CloseUnblocksRead(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "rep1", Name: "repeater", Running: true})
	release := make(chan struct{})
	fake.ExecHandler = func(string, []string, io.Reader, io.Writer, io.Writer) int {
		<-release
		return 0
	}
	t.Cleanup(func() { close(release) })
	c := newClient(t, fake)
	ex, err := c.Exec(testContext(t), "rep1", []string{"sleep"}, io.Discard)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ex.Read(make([]byte, 8))
		done <- err
	}()
	ex.Close()
	if err := <-done; err == nil {
		t.Error("Read after Close returned no error")
	}
}

func TestExec_Errors(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "stopped", Name: "stopped", Running: false})
	c := newClient(t, fake)

	if _, err := c.Exec(testContext(t), "missing", []string{"true"}, io.Discard); !docker.IsNotFound(err) {
		t.Errorf("exec in a missing container: error = %v, want not found", err)
	}
	if _, err := c.Exec(testContext(t), "stopped", []string{"true"}, io.Discard); !docker.IsConflict(err) {
		t.Errorf("exec in a stopped container: error = %v, want conflict", err)
	}
}

func TestExec_ReadStreamTellsStderrApart(t *testing.T) {
	fake := dockertest.New(t)
	fake.AddContainer(dockertest.Container{ID: "rep1", Name: "repeater", Running: true})
	fake.ExecHandler = func(_ string, _ []string, _ io.Reader, stdout, stderr io.Writer) int {
		io.WriteString(stdout, "greeting")
		io.WriteString(stderr, "notice")
		return 0
	}
	c := newClient(t, fake)
	ex, err := c.Exec(testContext(t), "rep1", []string{"x"}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer ex.Close()

	var stdout, stderr string
	buf := make([]byte, 64)
	for {
		n, isStderr, err := ex.ReadStream(buf)
		if isStderr {
			stderr += string(buf[:n])
		} else {
			stdout += string(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadStream: %v", err)
		}
	}
	if stdout != "greeting" || stderr != "notice" {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}
}

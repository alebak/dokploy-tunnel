package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestDemux_Read(t *testing.T) {
	tests := []struct {
		name       string
		input      [][]byte
		wantStdout string
		wantStderr string
		wantErr    error
	}{
		{"empty stream", nil, "", "", nil},
		{"stdout only", [][]byte{frame(1, "hello "), frame(1, "world")}, "hello world", "", nil},
		{"interleaved", [][]byte{frame(2, "warn\n"), frame(1, "data"), frame(2, "more\n"), frame(1, "!")}, "data!", "warn\nmore\n", nil},
		{"empty frames are skipped", [][]byte{frame(1, ""), frame(2, ""), frame(1, "x")}, "x", "", nil},
		{"stdin frames are ignored", [][]byte{frame(0, "echo"), frame(1, "x")}, "x", "", nil},
		{"truncated header", [][]byte{frame(1, "ok"), {1, 0, 0}}, "ok", "", io.ErrUnexpectedEOF},
		{"truncated payload", [][]byte{frame(1, "ok")[:9]}, "o", "", io.ErrUnexpectedEOF},
		{"unknown stream", [][]byte{frame(7, "?")}, "", "", errBadFrame},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			// One byte per read exercises frames split across reads.
			d := &demux{r: iotest.OneByteReader(bytes.NewReader(bytes.Join(tt.input, nil))), stderr: &stderr}
			got, err := io.ReadAll(d)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if string(got) != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestDemux_SmallReadBuffer(t *testing.T) {
	d := &demux{r: bytes.NewReader(frame(1, "abcdefgh")), stderr: io.Discard}
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := d.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != "abcdefgh" {
		t.Errorf("got %q", got)
	}
}

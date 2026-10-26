package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var noOpCommand = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
	0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
}

var loopingCommand = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b,
}

func TestRunExecutesWASICommandWithoutHostCapabilities(t *testing.T) {
	r := newTestRunner(t)
	module := writeModule(t, noOpCommand)

	output, err := r.Run(context.Background(), module, []byte(`{"method":"search"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 0 {
		t.Fatalf("expected empty output, got %q", output)
	}
}

func TestRunRejectsInvalidWasm(t *testing.T) {
	r := newTestRunner(t)
	module := writeModule(t, []byte("not wasm"))
	if _, err := r.Run(context.Background(), module, nil); err == nil {
		t.Fatal("expected invalid module error")
	}
}

func TestRunStopsModuleAtDeadline(t *testing.T) {
	r := newTestRunner(t)
	module := writeModule(t, loopingCommand)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, module, nil); err == nil {
		t.Fatal("expected infinite module to stop at the deadline")
	}
}

func TestRunRejectsOversizedInputBeforeReadingModule(t *testing.T) {
	r := newTestRunner(t)
	if _, err := r.Run(context.Background(), filepath.Join(t.TempDir(), "missing.wasm"), make([]byte, maxInputBytes+1)); err == nil {
		t.Fatal("expected input size error")
	}
}

func TestLimitedBufferRejectsOversizedOutput(t *testing.T) {
	buffer := &limitedBuffer{limit: 3}
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("d")); err == nil {
		t.Fatal("expected output size error")
	}
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("close runner: %v", err)
		}
	})
	return r
}

func writeModule(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.wasm")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

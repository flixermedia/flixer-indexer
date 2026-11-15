package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.bin")
	content := []byte("exact file content")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)
	got, err := hashFile(context.Background(), path)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("hashFile = %q, %v; want %x", got, err, want)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashFile(cancelled, path); err != context.Canceled {
		t.Fatalf("cancelled hash error = %v", err)
	}

	link := filepath.Join(t.TempDir(), "media-link.bin")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFile(context.Background(), link); err == nil {
		t.Fatal("symbolic links must not be hashed")
	}
}

package themes

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	releaseBase          = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	refreshAge           = 14 * 24 * time.Hour
	maxBinary            = 100 << 20
	checksumFile         = "SHA2-256SUMS"
	maxChecksumSizeBytes = 64 << 10
)

// YTDLP locates yt-dlp, downloading the project's standalone release into the
// cache when none is installed, so no setup is needed in dev or in Docker.
// YTDLP_PATH overrides everything.
type YTDLP struct {
	Dir       string
	UserAgent string
	HTTP      *http.Client
	BaseURL   string
	// Lookup finds an installed binary; tests replace it.
	Lookup func(string) (string, error)

	mu sync.Mutex
}

func NewYTDLP(dir, userAgent string) *YTDLP {
	return &YTDLP{Dir: dir, UserAgent: userAgent, HTTP: &http.Client{Timeout: 5 * time.Minute}, BaseURL: releaseBase, Lookup: exec.LookPath}
}

// Asset names the release file for a platform.
func Asset(goos, goarch string, musl bool) (string, error) {
	switch goos {
	case "windows":
		return "yt-dlp.exe", nil
	case "darwin":
		return "yt-dlp_macos", nil
	case "linux":
		name := "yt-dlp_linux"
		if musl {
			name = "yt-dlp_musllinux"
		}
		switch goarch {
		case "amd64":
			return name, nil
		case "arm64":
			return name + "_aarch64", nil
		}
	}
	return "", fmt.Errorf("no yt-dlp build for %s/%s", goos, goarch)
}

func isMusl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	return len(matches) > 0
}

// Ensure returns a usable yt-dlp path.
func (y *YTDLP) Ensure(ctx context.Context) (string, error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if p := os.Getenv("YTDLP_PATH"); p != "" {
		return p, nil
	}
	if p, err := y.Lookup("yt-dlp"); err == nil {
		return p, nil
	}
	asset, err := Asset(runtime.GOOS, runtime.GOARCH, isMusl())
	if err != nil {
		return "", err
	}
	dest := filepath.Join(y.Dir, "yt-dlp")
	if runtime.GOOS == "windows" {
		dest += ".exe"
	}
	st, statErr := os.Stat(dest)
	if statErr == nil && time.Since(st.ModTime()) < refreshAge {
		return dest, nil
	}
	if err := y.download(ctx, asset, dest); err != nil {
		if statErr == nil {
			return dest, nil // a stale binary beats none
		}
		return "", err
	}
	return dest, nil
}

func (y *YTDLP) download(ctx context.Context, asset, dest string) error {
	if err := os.MkdirAll(y.Dir, 0o755); err != nil {
		return err
	}
	expectedHash, err := y.fetchExpectedChecksum(ctx, asset)
	if err != nil {
		return fmt.Errorf("verify yt-dlp checksum: %w", err)
	}

	baseURL := strings.TrimRight(y.BaseURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+asset, nil)
	if err != nil {
		return err
	}
	if y.UserAgent != "" {
		req.Header.Set("User-Agent", y.UserAgent)
	}
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download yt-dlp: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download yt-dlp: status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(y.Dir, ".yt-dlp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()

	hasher := sha256.New()
	writer := io.MultiWriter(tmp, hasher)
	n, err := io.Copy(writer, io.LimitReader(resp.Body, maxBinary+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n == 0 || n > maxBinary) {
		err = errors.New("downloaded yt-dlp has an unexpected size")
	}
	if err == nil {
		actualHash := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(actualHash, expectedHash) {
			err = fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, actualHash, expectedHash)
		}
	}
	if err == nil {
		err = os.Chmod(name, 0o755)
	}
	if err == nil {
		err = os.Rename(name, dest)
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (y *YTDLP) fetchExpectedChecksum(ctx context.Context, asset string) (string, error) {
	baseURL := strings.TrimRight(y.BaseURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+checksumFile, nil)
	if err != nil {
		return "", err
	}
	if y.UserAgent != "" {
		req.Header.Set("User-Agent", y.UserAgent)
	}
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", checksumFile, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: status %d", checksumFile, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumSizeBytes))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", checksumFile, err)
	}
	return parseChecksum(string(body), asset)
}

func parseChecksum(content, asset string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := fields[0]
		fileName := strings.TrimPrefix(fields[1], "*")
		if fileName == asset {
			if len(hash) != 64 {
				return "", fmt.Errorf("invalid sha256 hash length for %s", asset)
			}
			return strings.ToLower(hash), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("asset %q not found in %s", asset, checksumFile)
}

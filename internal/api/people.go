package api

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

// Person photos come from provider-supplied metadata, so only these hosts are ever requested.
var personPhotoHosts = map[string]bool{
	"image.tmdb.org": true, "artworks.thetvdb.com": true, "thetvdb.com": true, "www.thetvdb.com": true, "i.discogs.com": true,
}

const (
	maxPersonPhoto  = 5 << 20
	personCacheDir  = "people"
	personFetchWait = 15 * time.Second
)

var (
	personIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	personHTTP      = &http.Client{
		Timeout:       personFetchWait,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	personLocks sync.Map
)

type personImageRequest struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func allowedPersonPhoto(raw string) (string, bool) {
	if len(raw) > 500 {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || !personPhotoHosts[u.Hostname()] {
		return "", false
	}
	return u.String(), true
}

// personImage serves a person photo, downloading it into the cache on first use
// so later requests never reach the metadata provider.
func (s *Server) personImage(w http.ResponseWriter, r *http.Request) {
	var req personImageRequest
	if !decode(w, r, &req) {
		return
	}
	photo, ok := allowedPersonPhoto(req.URL)
	if !ok || !personIDPattern.MatchString(req.ID) || s.CachePath == "" {
		writeError(w, http.StatusBadRequest, "person id and an allowed photo URL are required")
		return
	}
	base := filepath.Join(s.CachePath, personCacheDir, strings.ToLower(req.ID)+".jpg")

	path, ctype := cachedPersonPhoto(base)
	if path == "" {
		lock, _ := personLocks.LoadOrStore(base, &sync.Mutex{})
		mu := lock.(*sync.Mutex)
		mu.Lock()
		path, ctype = cachedPersonPhoto(base)
		if path == "" {
			var err error
			path, ctype, err = downloadPersonPhoto(r, photo, base)
			if err != nil {
				s.Log.Warn("person photo download failed", "error", err.Error())
				mu.Unlock()
				personLocks.Delete(base)
				writeError(w, http.StatusBadGateway, "the image could not be loaded")
				return
			}
		}
		mu.Unlock()
		personLocks.Delete(base)
	}

	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func cachedPersonPhoto(base string) (string, string) {
	if info, err := os.Stat(base); err == nil && info.Mode().IsRegular() {
		return base, "image/jpeg"
	}
	return "", ""
}

func downloadPersonPhoto(r *http.Request, photo, base string) (string, string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, photo, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "flixer-indexer")
	resp, err := personHTTP.Do(req)
	if err != nil {
		return "", "", errors.New("download failed")
	}
	defer resp.Body.Close()
	ctype := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if resp.StatusCode != http.StatusOK || (ctype != "image/jpeg" && ctype != "image/png" && ctype != "image/webp") {
		return "", "", errors.New("unexpected response")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPersonPhoto+1))
	if err != nil || len(data) == 0 || len(data) > maxPersonPhoto {
		return "", "", errors.New("invalid image body")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", "", errors.New("invalid image body")
	}
	dest := base
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".flixer-*")
	if err != nil {
		return "", "", err
	}
	werr := jpeg.Encode(tmp, decoded, &jpeg.Options{Quality: 90})
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), dest) != nil {
		os.Remove(tmp.Name())
		return "", "", errors.New("could not store image")
	}
	return dest, "image/jpeg", nil
}

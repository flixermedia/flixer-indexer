package api

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Person photos come from provider-supplied metadata, so only these hosts are ever requested.
var personPhotoHosts = map[string]bool{
	"image.tmdb.org": true, "artworks.thetvdb.com": true, "thetvdb.com": true, "www.thetvdb.com": true, "i.discogs.com": true,
}

const (
	maxPersonPhoto  = 5 << 20
	personCacheDir  = "_people"
	personFetchWait = 15 * time.Second
)

var (
	personHTTP = &http.Client{
		Timeout:       personFetchWait,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	personMimes = map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp"}
	personLocks sync.Map
)

type personImageRequest struct {
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

// personImage serves a person photo, downloading it into the metadata cache on first use
// so later requests never reach the metadata provider.
func (s *Server) personImage(w http.ResponseWriter, r *http.Request) {
	var req personImageRequest
	if !decode(w, r, &req) {
		return
	}
	photo, ok := allowedPersonPhoto(req.URL)
	if !ok || s.CachePath == "" {
		writeError(w, http.StatusBadRequest, "url is not an allowed person photo")
		return
	}
	sum := sha1.Sum([]byte(photo))
	base := filepath.Join(s.CachePath, personCacheDir, hex.EncodeToString(sum[:]))

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
	for ctype, ext := range personMimes {
		if info, err := os.Stat(base + "." + ext); err == nil && info.Mode().IsRegular() {
			return base + "." + ext, ctype
		}
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
	ext, ok := personMimes[ctype]
	if resp.StatusCode != http.StatusOK || !ok {
		return "", "", errors.New("unexpected response")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPersonPhoto+1))
	if err != nil || len(data) == 0 || len(data) > maxPersonPhoto {
		return "", "", errors.New("invalid image body")
	}
	dest := base + "." + ext
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".flixer-*")
	if err != nil {
		return "", "", err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), dest) != nil {
		os.Remove(tmp.Name())
		return "", "", errors.New("could not store image")
	}
	return dest, ctype, nil
}

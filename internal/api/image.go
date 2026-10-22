package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// imageTypes are the only files the image endpoint will serve.
var imageTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif",
}

const maxServedImage = 25 << 20

type imageRequest struct {
	Path string `json:"path"`
}

// image serves an artwork file the indexer saved. The path travels in the signed
// body, and is only honoured inside the metadata cache or a library folder, so
// this cannot be used to read arbitrary files.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if !decode(w, r, &req) {
		return
	}
	contentType, ok := imageTypes[strings.ToLower(filepath.Ext(req.Path))]
	if req.Path == "" || !ok {
		writeError(w, http.StatusBadRequest, "path must be an image file path")
		return
	}
	// Images saved before cache paths were made absolute are relative to the indexer's working directory.
	if abs, err := filepath.Abs(req.Path); err == nil {
		req.Path = abs
	}
	real, err := filepath.EvalSymlinks(req.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if !s.imageAllowed(r, real) {
		writeError(w, http.StatusForbidden, "image is outside the indexed folders")
		return
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if info.Size() > maxServedImage {
		writeError(w, http.StatusRequestEntityTooLarge, "image too large")
		return
	}
	f, err := os.Open(real)
	if err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(real), info.ModTime(), f)
}

func (s *Server) imageAllowed(r *http.Request, real string) bool {
	if s.CachePath != "" && within(s.CachePath, real) {
		return true
	}
	libs, err := s.Pipeline.Libraries(r.Context())
	if err != nil {
		return false
	}
	for _, lib := range libs {
		for _, folder := range lib.Folders {
			if within(folder.AbsolutePath, real) {
				return true
			}
		}
	}
	return false
}

// within reports whether target is inside root, after resolving symlinks in root.
func within(root, target string) bool {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolved, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

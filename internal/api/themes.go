package api

import (
	"net/http"
	"os"

	"github.com/flixermedia/flixer-indexer/internal/themes"
)

const maxThemeQueue = 500

// theme serves an item's theme song if one has been downloaded. The path comes
// from the signed body but is only used to look up files this service wrote.
func (s *Server) theme(w http.ResponseWriter, r *http.Request) {
	var task themes.Task
	if !decode(w, r, &task) {
		return
	}
	if s.Pipeline.Themes == nil || task.LibraryID == "" || task.Path == "" {
		writeError(w, http.StatusNotFound, "no theme")
		return
	}
	lib, err := s.Pipeline.Library(r.Context(), task.LibraryID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no theme")
		return
	}
	path := s.Pipeline.Themes.Locate(lib, task, s.Pipeline.FolderRoot(lib, task.Path))
	if path == "" {
		writeError(w, http.StatusNotFound, "no theme")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "no theme")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "no theme")
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, themes.FileName, info.ModTime(), f)
}

type queueThemesRequest struct {
	Items []themes.Task `json:"items"`
}

// queueThemes enqueues downloads for existing items; flixer calls it from the admin sync button.
func (s *Server) queueThemes(w http.ResponseWriter, r *http.Request) {
	var req queueThemesRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Items) > maxThemeQueue {
		writeError(w, http.StatusBadRequest, "too many items")
		return
	}
	if !s.Pipeline.ThemesEnabled() {
		writeError(w, http.StatusConflict, "theme downloads are disabled")
		return
	}
	queued := 0
	for _, task := range req.Items {
		if task.LibraryID == "" || task.Path == "" || task.TMDBID == "" {
			continue
		}
		if s.Pipeline.QueueTheme(r.Context(), task) {
			queued++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

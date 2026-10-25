package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/subtitles"
)

// subtitleRequest identifies a video and, depending on the endpoint, what to do with it.
type subtitleRequest struct {
	Path              string            `json:"path"`
	RootID            string            `json:"rootId,omitempty"`
	RelativePath      string            `json:"relativePath,omitempty"`
	Type              string            `json:"type,omitempty"`
	Title             string            `json:"title,omitempty"`
	SeriesTitle       string            `json:"seriesTitle,omitempty"`
	Year              int               `json:"year,omitempty"`
	Season            *int              `json:"season,omitempty"`
	Episode           *int              `json:"episode,omitempty"`
	ProviderIDs       map[string]string `json:"providerIds,omitempty"`
	SeriesProviderIDs map[string]string `json:"seriesProviderIds,omitempty"`
	Query             string            `json:"query,omitempty"`
	Languages         []string          `json:"languages,omitempty"`
	FileID            int               `json:"fileId,omitempty"`
	Language          string            `json:"language,omitempty"`
	File              string            `json:"file,omitempty"`
}

// videoPath resolves the request path and only accepts a regular file inside a library folder.
func (s *Server) videoPath(w http.ResponseWriter, r *http.Request, path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "path must be an absolute media file path")
		return "", false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "media file not found")
		return "", false
	}
	libs, err := s.Pipeline.Libraries(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not load libraries")
		return "", false
	}
	allowed := false
	for _, lib := range libs {
		for _, folder := range lib.Folders {
			if folder.AbsolutePath != "" && within(folder.AbsolutePath, real) {
				allowed = true
			}
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "media file is outside the indexed folders")
		return "", false
	}
	if info, err := os.Stat(real); err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "media file not found")
		return "", false
	}
	return real, true
}

func (s *Server) target(req subtitleRequest, path string) subtitles.Target {
	return subtitles.Target{Video: path, Fallback: subtitles.FallbackDir(s.Pipeline.SubtitleDir, req.RootID, req.RelativePath)}
}

func (s *Server) subtitleFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, subtitles.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "OpenSubtitles is not configured on the indexer")
	case errors.Is(err, subtitles.ErrCredentials):
		writeError(w, http.StatusFailedDependency, "Downloading subtitles needs valid OpenSubtitles username and password on the indexer")
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EROFS):
		s.Log.Warn("subtitle folder is not writable", "error", err)
		writeError(w, http.StatusFailedDependency, "the media folder is not writable by the indexer")
	case errors.Is(err, subtitles.ErrQuota):
		writeError(w, http.StatusTooManyRequests, "OpenSubtitles download quota reached; try again later")
	case errors.Is(err, subtitles.ErrNotFound):
		writeError(w, http.StatusNotFound, "no subtitles found")
	case errors.Is(err, subtitles.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.Log.Warn("subtitle request failed", "error", err)
		writeError(w, http.StatusBadGateway, "OpenSubtitles request failed")
	}
}

func (s *Server) subtitleList(w http.ResponseWriter, r *http.Request) {
	var req subtitleRequest
	if !decode(w, r, &req) {
		return
	}
	path, ok := s.videoPath(w, r, req.Path)
	if !ok {
		return
	}
	client, _, _ := s.Pipeline.SubtitleConfig()
	tracks, err := subtitles.List(s.target(req, path))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the media folder")
		return
	}
	if tracks == nil {
		tracks = []subtitles.Track{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tracks": tracks, "configured": client != nil})
}

func (s *Server) subtitleQuery(req subtitleRequest, path string) subtitles.Query {
	item := models.Item{
		Type: req.Type, ProviderIDs: req.ProviderIDs, SeasonNumber: req.Season, EpisodeNumber: req.Episode,
		Metadata: models.Metadata{Title: req.Title, Year: req.Year},
	}
	q := pipeline.SubtitleQuery(item, models.Hints{SeriesTitle: req.SeriesTitle, SeriesIDs: req.SeriesProviderIDs})
	if text := strings.TrimSpace(req.Query); text != "" {
		// A typed title replaces the ids, so it can find a different release.
		q.Text, q.IMDB, q.TMDB, q.ParentIMD, q.ParentTMD = text, "", "", "", ""
	} else if hash, err := subtitles.Hash(path); err == nil {
		q.Hash = hash
	}
	return q
}

func (s *Server) subtitleSearch(w http.ResponseWriter, r *http.Request) {
	var req subtitleRequest
	if !decode(w, r, &req) {
		return
	}
	path, ok := s.videoPath(w, r, req.Path)
	if !ok {
		return
	}
	for _, lang := range req.Languages {
		if !subtitles.ValidLanguage(lang) {
			writeError(w, http.StatusBadRequest, "invalid language code")
			return
		}
	}
	client, wanted, _ := s.Pipeline.SubtitleConfig()
	q := s.subtitleQuery(req, path)
	q.Languages = req.Languages
	if len(q.Languages) == 0 {
		q.Languages = wanted
	}
	results, err := client.Search(r.Context(), q)
	if err != nil {
		s.subtitleFailure(w, err)
		return
	}
	if results == nil {
		results = []subtitles.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) subtitleDownload(w http.ResponseWriter, r *http.Request) {
	var req subtitleRequest
	if !decode(w, r, &req) {
		return
	}
	path, ok := s.videoPath(w, r, req.Path)
	if !ok {
		return
	}
	if req.FileID <= 0 || !subtitles.ValidLanguage(req.Language) {
		writeError(w, http.StatusBadRequest, "fileId and language are required")
		return
	}
	client, _, _ := s.Pipeline.SubtitleConfig()
	data, err := client.Download(r.Context(), req.FileID)
	if err != nil {
		s.subtitleFailure(w, err)
		return
	}
	saved, err := subtitles.Save(s.target(req, path), req.Language, data)
	if err != nil {
		s.subtitleFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"file": filepath.Base(saved)})
}

func (s *Server) subtitleDelete(w http.ResponseWriter, r *http.Request) {
	var req subtitleRequest
	if !decode(w, r, &req) {
		return
	}
	path, ok := s.videoPath(w, r, req.Path)
	if !ok {
		return
	}
	if err := subtitles.Remove(s.target(req, path), req.File); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "subtitle not found")
			return
		}
		s.subtitleFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

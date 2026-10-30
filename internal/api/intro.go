package api

import (
	"net/http"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

type queueIntroRequest struct {
	LibraryID string `json:"libraryId"`
	Type      string `json:"type"`
}

func (s *Server) queueIntroSegments(w http.ResponseWriter, r *http.Request) {
	var req queueIntroRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LibraryID == "" || (req.Type != models.ItemMovie && req.Type != models.ItemEpisode) {
		writeError(w, http.StatusBadRequest, "libraryId and a valid type are required")
		return
	}
	if _, err := s.Pipeline.Library(r.Context(), req.LibraryID); err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	queued, err := s.Pipeline.QueueIntroBackfill(r.Context(), req.LibraryID, req.Type)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued})
}

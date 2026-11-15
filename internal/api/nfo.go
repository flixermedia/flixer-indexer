package api

import (
	"net/http"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

func (s *Server) writeNFO(w http.ResponseWriter, r *http.Request) {
	var request models.NFOWriteRequest
	if !decode(w, r, &request) {
		return
	}
	if request.LibraryID == "" || request.Path == "" || request.ItemType == "" {
		writeError(w, http.StatusBadRequest, "libraryId, itemType and path are required")
		return
	}
	if err := s.Pipeline.WriteNFO(r.Context(), request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"written": true})
}

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/flixermedia/flixer-indexer/internal/bios"
	"github.com/flixermedia/flixer-indexer/internal/httpx"
)

type personBioRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// personBio returns a biography and photo for a person. Music artists come from Discogs when it is enabled,
// everyone else (and artists Discogs does not know) from Wikipedia. The caller stores the result.
func (s *Server) personBio(w http.ResponseWriter, r *http.Request) {
	var req personBioRequest
	if !decode(w, r, &req) {
		return
	}
	bio, err := s.lookupBio(r.Context(), req)
	switch {
	case errors.Is(err, httpx.ErrNotFound):
		writeError(w, http.StatusNotFound, "no biography found")
	case err != nil:
		s.Log.Warn("biography lookup failed", "error", err.Error())
		writeError(w, http.StatusBadGateway, "the biography could not be loaded")
	default:
		writeJSON(w, http.StatusOK, bio)
	}
}

func (s *Server) lookupBio(ctx context.Context, req personBioRequest) (*bios.Bio, error) {
	var discogsErr error
	if req.Kind == bios.KindArtist && s.Pipeline != nil {
		if reg := s.Pipeline.Registry(); reg != nil {
			artist, err := reg.Artist(ctx, req.Name)
			if err == nil {
				description := ""
				if artist.RealName != "" && !strings.EqualFold(artist.RealName, artist.Name) {
					description = artist.RealName
				}
				return &bios.Bio{Description: description, Biography: artist.Profile, URL: artist.URL, Source: "Discogs", ImageURL: artist.ImageURL}, nil
			}
			if !errors.Is(err, httpx.ErrNotFound) {
				discogsErr = err
			}
		}
	}
	bio, err := s.Bios.Lookup(ctx, req.Name, req.Kind)
	if err != nil && discogsErr != nil && errors.Is(err, httpx.ErrNotFound) {
		return nil, discogsErr
	}
	return bio, err
}

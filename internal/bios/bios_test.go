package bios

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
)

func TestLookupSkipsNamesakes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/Zach_Bryan"):
			w.Write([]byte(`{"type":"disambiguation","title":"Zach Bryan","description":"Topics referred to by the same term","extract":"x"}`))
		case strings.HasSuffix(r.URL.Path, "/Zach_Bryan_(musician)"):
			w.Write([]byte(`{"type":"standard","title":"Zach Bryan","description":"American singer-songwriter","extract":"Zachary Bryan is a singer.","content_urls":{"desktop":{"page":"https://en.wikipedia.org/wiki/Zach_Bryan"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New("test").WithBase(srv.URL + "/")

	bio, err := c.Lookup(context.Background(), "Zach Bryan", KindArtist)
	if err != nil || bio.Description != "American singer-songwriter" || bio.Source != "Wikipedia" {
		t.Fatalf("bio = %+v, %v", bio, err)
	}
	if _, err := c.Lookup(context.Background(), "Nobody Here", KindArtist); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("missing person: %v", err)
	}
	// A musician's article must not be used for an actor lookup.
	if _, err := c.Lookup(context.Background(), "Zach Bryan", "actor"); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("kind mismatch: %v", err)
	}
}

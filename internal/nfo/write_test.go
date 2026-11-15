package nfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteUpdatesKnownFieldsAndPreservesUnknownXML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "episode.nfo")
	original := `<?xml version="1.0"?><episodedetails><title>Wrong title</title><plot>Keep this plot</plot><imdbid>tt12158352</imdbid><uniqueid type="tmdb" default="true">1703939</uniqueid><custom value="keep"><nested>also keep</nested></custom></episodedetails>`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, "episodedetails", map[string]string{"title": "The Future is Borax", "season": "15", "episode": "22"}, map[string]string{"imdb": "tt99999999", "tmdb": "12345"}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if data.Metadata.Title != "The Future is Borax" || data.IDs["imdb"] != "tt99999999" || data.IDs["tmdb"] != "12345" {
		t.Fatalf("updated NFO = %+v ids=%v", data.Metadata, data.IDs)
	}
	if intValue(data.Season) != 15 || intValue(data.Episode) != 22 {
		t.Fatalf("episode coordinates = %v/%v", data.Season, data.Episode)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"Keep this plot", `custom value="keep"`, "also keep"} {
		if !strings.Contains(string(updated), preserved) {
			t.Errorf("NFO lost unknown content %q: %s", preserved, updated)
		}
	}
}

func TestWriteCreatesMissingSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "movie.nfo")
	if err := Write(path, "movie", map[string]string{"title": "A Movie", "year": "2024"}, map[string]string{"tmdb": "42"}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path)
	if err != nil || data.Metadata.Title != "A Movie" || data.Metadata.Year != 2024 || data.IDs["tmdb"] != "42" {
		t.Fatalf("created NFO = %+v, %v", data, err)
	}
}

func TestWriteRejectsSymlinkAndMalformedNFO(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(directory, "outside.nfo")
	if err := os.WriteFile(outside, []byte(`<movie><title>Outside</title></movie>`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked.nfo")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, "movie", map[string]string{"title": "Changed"}, nil, nil); err == nil {
		t.Fatal("expected symlink write rejection")
	}
	malformed := filepath.Join(directory, "malformed.nfo")
	if err := os.WriteFile(malformed, []byte("not xml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(malformed, "movie", map[string]string{"title": "Changed"}, nil, nil); err == nil {
		t.Fatal("expected malformed NFO rejection")
	}
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

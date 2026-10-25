package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

func touch(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		p := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func collect(t *testing.T, lib models.Library) []models.Work {
	t.Helper()
	var out []models.Work
	err := Scan(context.Background(), lib, func(w models.Work) error {
		out = append(out, w)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lib(typ, root string) models.Library {
	return models.Library{ID: "lib1", Type: typ, Folders: []models.Folder{{RootID: "r", Path: "Lib", AbsolutePath: root}}}
}

func count(ws []models.Work, typ string) int {
	n := 0
	for _, w := range ws {
		if w.Item.Type == typ {
			n++
		}
	}
	return n
}

func TestMovies(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"The Matrix (1999) [tmdbid-603]/The Matrix (1999).mkv",
		"The Matrix (1999) [tmdbid-603]/Trailers/trailer.mkv",
		"Loose Movie (2010).mp4",
		"notes.txt")
	ws := collect(t, lib(models.LibraryMovies, root))
	if len(ws) != 2 {
		t.Fatalf("got %d items: %+v", len(ws), ws)
	}
	var matrix models.Work
	for _, w := range ws {
		if w.Hints.Title == "The Matrix" {
			matrix = w
		}
	}
	if matrix.Hints.Year != 1999 || matrix.Item.ProviderIDs["tmdb"] != "603" || matrix.Item.RelativePath != "Lib/The Matrix (1999) [tmdbid-603]/The Matrix (1999).mkv" {
		t.Errorf("matrix = %+v", matrix)
	}
}

func TestShows(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"Show (2020)/Season 1/Show S01E01 Pilot.mkv",
		"Show (2020)/Season 1/Show S01E02.mkv",
		"Show (2020)/Season 2/Show S02E01.mkv",
		"Show (2020)/Specials/ignored.txt")
	ws := collect(t, lib(models.LibraryTVShows, root))
	if count(ws, models.ItemSeries) != 1 || count(ws, models.ItemSeason) != 2 || count(ws, models.ItemEpisode) != 3 {
		t.Fatalf("unexpected items: %d", len(ws))
	}
	for _, w := range ws {
		if w.Item.Type == models.ItemEpisode && (w.Hints.SeriesTitle != "Show" || w.Item.SeasonNumber == nil) {
			t.Errorf("episode = %+v", w)
		}
		switch w.Item.Type {
		case models.ItemSeason:
			if w.Item.ParentPath != filepath.Join(root, "Show (2020)") {
				t.Errorf("season parent = %q", w.Item.ParentPath)
			}
		case models.ItemEpisode:
			want := filepath.Join(root, "Show (2020)", fmt.Sprintf("Season %d", *w.Item.SeasonNumber))
			if w.Item.ParentPath != want {
				t.Errorf("episode parent = %q, want %q", w.Item.ParentPath, want)
			}
		}
	}
}

func TestFlatShowEpisodesLinkToSyntheticSeason(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "Flat/Flat S01E01.mkv", "Flat/Flat S01E02.mkv")
	ws := collect(t, lib(models.LibraryTVShows, root))
	var seasonPath string
	for _, w := range ws {
		if w.Item.Type == models.ItemSeason {
			seasonPath = w.Item.Path
		}
	}
	if seasonPath == "" {
		t.Fatal("no season emitted")
	}
	for _, w := range ws {
		if w.Item.Type == models.ItemEpisode && w.Item.ParentPath != seasonPath {
			t.Errorf("episode parent = %q, want %q", w.Item.ParentPath, seasonPath)
		}
	}
}

func TestMusicWithDiscs(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"Artist/Album One (2001)/01 - First.flac",
		"Artist/Album One (2001)/02 - Second.flac",
		"Artist/Album Two/CD 1/01 - A.flac",
		"Artist/Album Two/CD 2/01 - B.flac")
	ws := collect(t, lib(models.LibraryMusic, root))
	if count(ws, models.ItemAlbum) != 2 || count(ws, models.ItemTrack) != 4 {
		t.Fatalf("items: %d", len(ws))
	}
	for _, w := range ws {
		if w.Item.Type == models.ItemAlbum && w.Hints.Artist != "Artist" {
			t.Errorf("album artist = %q", w.Hints.Artist)
		}
		if w.Item.Type == models.ItemTrack && w.Hints.Title == "B" && w.Item.Metadata.DiscNumber != 2 {
			t.Errorf("disc = %d", w.Item.Metadata.DiscNumber)
		}
	}
}

func TestDescribe(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "Show/Season 1/Show S01E01.mkv", "Show/Season 1/Show S01E02.mkv")
	l := lib(models.LibraryTVShows, root)
	path := filepath.Join(root, "Show", "Season 1", "Show S01E02.mkv")
	w, err := Describe(context.Background(), l, path, models.ItemEpisode)
	if err != nil || w.Item.EpisodeNumber == nil || *w.Item.EpisodeNumber != 2 {
		t.Fatalf("describe = %+v, %v", w, err)
	}
	if _, err := Describe(context.Background(), l, filepath.Join(root, "Nope.mkv"), models.ItemMovie); err == nil {
		t.Error("expected error")
	}
}

func TestMixed(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "Film (2000)/Film (2000).mkv", "Series/Season 1/S01E01.mkv", "Band/Record/01 - x.mp3")
	ws := collect(t, lib(models.LibraryMixed, root))
	if count(ws, models.ItemMovie) != 1 || count(ws, models.ItemEpisode) != 1 || count(ws, models.ItemAlbum) != 1 {
		t.Fatalf("items: %+v", ws)
	}
}

func TestScanBranch(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"Show (2020)/Season 1/Show S01E01.mkv",
		"Show (2020)/Season 2/Show S02E01.mkv",
		"Other (2021)/Season 1/Other S01E01.mkv")
	l := lib(models.LibraryTVShows, root)
	var got []models.Work
	collectInto := func(w models.Work) error { got = append(got, w); return nil }

	if err := ScanBranch(context.Background(), l, filepath.Join(root, "Show (2020)"), collectInto); err != nil {
		t.Fatal(err)
	}
	if count(got, models.ItemSeries) != 1 || count(got, models.ItemSeason) != 2 || count(got, models.ItemEpisode) != 2 {
		t.Fatalf("series branch = %d items", len(got))
	}

	got = nil
	if err := ScanBranch(context.Background(), l, filepath.Join(root, "Show (2020)", "Season 2"), collectInto); err != nil {
		t.Fatal(err)
	}
	if count(got, models.ItemSeason) != 1 || count(got, models.ItemEpisode) != 1 || count(got, models.ItemSeries) != 0 {
		t.Fatalf("season branch = %d items", len(got))
	}

	if err := ScanBranch(context.Background(), l, filepath.Join(root, "Missing"), collectInto); err == nil {
		t.Error("expected an error for a path with no items")
	}
}

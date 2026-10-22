package subtitles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListSaveRemove(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Movie (2020).mkv")
	for _, name := range []string{"Movie (2020).mkv", "Movie (2020).en.srt", "Movie (2020).fr.forced.srt", "Other.en.srt", "Movie (2020).nfo"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tracks, err := List(video)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].Language != "en" || tracks[1].Language != "fr" || tracks[1].Label != "forced" {
		t.Fatalf("unexpected tracks: %+v", tracks)
	}
	if !Has(tracks, "eng") || Has(tracks, "de") {
		t.Fatal("Has should match by primary language")
	}

	saved, err := Save(video, "en", []byte("\ufeffhello"))
	if err != nil || filepath.Base(saved) != "Movie (2020).en.2.srt" {
		t.Fatalf("save = %q, %v", saved, err)
	}
	if data, _ := os.ReadFile(saved); string(data) != "hello" {
		t.Fatalf("BOM not stripped: %q", data)
	}
	if _, err := Save(video, "../x", nil); err == nil {
		t.Fatal("expected invalid language error")
	}

	if err := Remove(video, "../Other.en.srt"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if err := Remove(video, "Other.en.srt"); err == nil {
		t.Fatal("expected rejection of another video's subtitle")
	}
	if err := Remove(video, "Movie (2020).en.srt"); err != nil {
		t.Fatal(err)
	}
}

func TestHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.bin")
	if err := os.WriteFile(path, make([]byte, 200<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Hash(path)
	if err != nil || got != "0000000000032000" {
		t.Fatalf("hash = %q, %v", got, err)
	}
}

package naming

import "testing"

func TestParseTitle(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
		ids   map[string]string
	}{
		{"Inception (2010)", "Inception", 2010, nil},
		{"The.Matrix.1999.1080p.BluRay.x264-GRP", "The Matrix", 1999, nil},
		{"Blade Runner 2049 (2017) [tmdbid-335984]", "Blade Runner 2049", 2017, map[string]string{"tmdb": "335984"}},
		{"Movie Title [imdbid-tt0133093]", "Movie Title", 0, map[string]string{"imdb": "tt0133093"}},
		{"Some Movie 1080p BluRay", "Some Movie", 0, nil},
		{"2012", "2012", 0, nil},
		{"Big Buck Bunny", "Big Buck Bunny", 0, nil},
		{"Spider-Man - Homecoming (2017)", "Spider-Man - Homecoming", 2017, nil},
	}
	for _, c := range cases {
		got := ParseTitle(c.in)
		if got.Title != c.title || got.Year != c.year {
			t.Errorf("ParseTitle(%q) = %q (%d), want %q (%d)", c.in, got.Title, got.Year, c.title, c.year)
		}
		for k, v := range c.ids {
			if got.IDs[k] != v {
				t.Errorf("ParseTitle(%q) id %s = %q, want %q", c.in, k, got.IDs[k], v)
			}
		}
	}
}

func TestParseEpisode(t *testing.T) {
	two := 2
	cases := []struct {
		in        string
		dirSeason *int
		season    int
		number    int
		end       int
		title     string
		matched   bool
		hasSeason bool
	}{
		{"Show - S01E02 - Pilot", nil, 1, 2, 0, "Pilot", true, true},
		{"show.s02e05.720p.hdtv.x264", nil, 2, 5, 0, "", true, true},
		{"Show S01E01E02", nil, 1, 1, 2, "", true, true},
		{"Show - S03E04-E05 - Two Parter", nil, 3, 4, 5, "Two Parter", true, true},
		{"Show 1x07 Name", nil, 1, 7, 0, "Name", true, true},
		{"03 - Third", &two, 2, 3, 0, "Third", true, true},
		{"Episode 4", &two, 2, 4, 0, "", true, true},
		{"random file", nil, 0, 0, 0, "", false, false},
		{"03 - Third", nil, 0, 0, 0, "", false, false},
	}
	for _, c := range cases {
		got := ParseEpisode(c.in, c.dirSeason)
		if got.Matched != c.matched {
			t.Errorf("ParseEpisode(%q) matched = %v", c.in, got.Matched)
			continue
		}
		if !c.matched {
			continue
		}
		if got.Season == nil || *got.Season != c.season || got.Number != c.number {
			t.Errorf("ParseEpisode(%q) = season %v ep %d, want %d/%d", c.in, got.Season, got.Number, c.season, c.number)
		}
		if (got.EndNum != nil && *got.EndNum != c.end) || (got.EndNum == nil && c.end != 0) {
			t.Errorf("ParseEpisode(%q) end = %v, want %d", c.in, got.EndNum, c.end)
		}
		if got.Title != c.title {
			t.Errorf("ParseEpisode(%q) title = %q, want %q", c.in, got.Title, c.title)
		}
	}
}

func TestSeasonAndDiscDirs(t *testing.T) {
	for in, want := range map[string]int{"Season 01": 1, "season 2": 2, "S03": 3, "Specials": 0} {
		if got, ok := SeasonFromDir(in); !ok || got != want {
			t.Errorf("SeasonFromDir(%q) = %d, %v", in, got, ok)
		}
	}
	if _, ok := SeasonFromDir("Extras"); ok {
		t.Error("Extras is not a season")
	}
	if n, ok := DiscFromDir("CD2"); !ok || n != 2 {
		t.Errorf("DiscFromDir(CD2) = %d, %v", n, ok)
	}
}

func TestParseTrackAndAlbum(t *testing.T) {
	if tr := ParseTrack("01 - Intro"); tr.Number != 1 || tr.Title != "Intro" || tr.Disc != 0 {
		t.Errorf("track = %+v", tr)
	}
	if tr := ParseTrack("2-07. Song"); tr.Disc != 2 || tr.Number != 7 || tr.Title != "Song" {
		t.Errorf("track = %+v", tr)
	}
	if tr := ParseTrack("Just A Name"); tr.Number != 0 || tr.Title != "Just A Name" {
		t.Errorf("track = %+v", tr)
	}
	if a := ParseAlbum("Artist - Album (2001)"); a.Artist != "Artist" || a.Title != "Album" || a.Year != 2001 {
		t.Errorf("album = %+v", a)
	}
	if a := ParseAlbum("1999 - Old Record"); a.Title != "Old Record" || a.Year != 1999 {
		t.Errorf("album = %+v", a)
	}
}

func TestIsExtra(t *testing.T) {
	for _, name := range []string{"Movie-trailer", "extras", "Featurettes", "movie-sample"} {
		if !IsExtra(name) {
			t.Errorf("%q should be an extra", name)
		}
	}
	if IsExtra("Movie") {
		t.Error("Movie is not an extra")
	}
}

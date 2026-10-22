package models

import "testing"

func TestAssignKeys(t *testing.T) {
	one, two := 1, 2
	ids := map[string]string{"tvdb": "9", "tmdb": "100"}

	series := Item{Type: ItemSeries, Path: "/tv/Wednesday", ProviderIDs: ids}
	AssignKeys(&series, nil, "")
	season := Item{Type: ItemSeason, Path: "/tv/Wednesday/Season 1", ParentPath: "/tv/Wednesday", SeasonNumber: &one}
	AssignKeys(&season, ids, "")
	ep := Item{Type: ItemEpisode, Path: "/tv/Wednesday/Season 1/e2.mkv", ParentPath: "/tv/Wednesday/Season 1", SeasonNumber: &one, EpisodeNumber: &two}
	AssignKeys(&ep, ids, "")

	if series.ExternalKey != "tmdb:100" {
		t.Errorf("series key = %q", series.ExternalKey)
	}
	if season.ExternalKey != "tmdb:100:s1" || season.ParentExternalKey != "tmdb:100" {
		t.Errorf("season keys = %q / %q", season.ExternalKey, season.ParentExternalKey)
	}
	if ep.ExternalKey != "tmdb:100:s1:e2" || ep.ParentExternalKey != "tmdb:100:s1" {
		t.Errorf("episode keys = %q / %q", ep.ExternalKey, ep.ParentExternalKey)
	}
}

func TestAssignKeysFallsBackToPath(t *testing.T) {
	one := 1
	ep := Item{Type: ItemEpisode, Path: "/tv/X/S01E01.mkv", ParentPath: "/tv/X/Season 1", SeasonNumber: &one, EpisodeNumber: &one}
	AssignKeys(&ep, nil, "")
	if ep.ExternalKey != "path:/tv/X/S01E01.mkv" || ep.ParentExternalKey != "path:/tv/X/Season 1" {
		t.Errorf("keys = %q / %q", ep.ExternalKey, ep.ParentExternalKey)
	}
}

func TestAssignKeysPathIsRelativeToLibraryFolder(t *testing.T) {
	album := Item{Type: ItemAlbum, Path: "/media/Music/Artist/Album"}
	AssignKeys(&album, nil, "/media/Music")
	moved := Item{Type: ItemAlbum, Path: "/media/MusicTest/Artist/Album"}
	AssignKeys(&moved, nil, "/media/MusicTest")
	if album.ExternalKey != "path:Artist/Album" || moved.ExternalKey != album.ExternalKey {
		t.Errorf("keys = %q / %q", album.ExternalKey, moved.ExternalKey)
	}
	track := Item{Type: ItemTrack, Path: "/media/Music/Artist/Album/1.wav", ParentPath: "/media/Music/Artist/Album"}
	AssignKeys(&track, nil, "/media/Music")
	if track.ExternalKey != "path:Artist/Album/1.wav" || track.ParentExternalKey != "path:Artist/Album" {
		t.Errorf("track keys = %q / %q", track.ExternalKey, track.ParentExternalKey)
	}
}

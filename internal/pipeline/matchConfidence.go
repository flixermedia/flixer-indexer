package pipeline

import (
	"strings"
	"unicode"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

func assessMatch(work models.Work, explicitIDs map[string]string, result *providers.Result) (string, string) {
	item := work.Item
	if item.Type == models.ItemTrack {
		expected := []string{work.Hints.Artist, work.Hints.Album, work.Hints.Title}
		actual := []string{item.Metadata.Artist, item.Metadata.Album, item.Metadata.Title}
		matched := 0
		complete := true
		for i := range expected {
			if normalizedMatchText(expected[i]) == "" || normalizedMatchText(actual[i]) == "" {
				complete = false
				continue
			}
			if !sameMatchText(expected[i], actual[i]) {
				return models.MatchConfidenceLow, []string{"artist_conflict", "album_conflict", "title_conflict"}[i]
			}
			matched++
		}
		if complete && matched == len(expected) {
			return models.MatchConfidenceHigh, "track_tags_match"
		}
		if matched > 0 || len(item.ProviderIDs) > 0 {
			return models.MatchConfidenceMedium, "partial_track_evidence"
		}
		return models.MatchConfidenceUnidentified, "no_provider_match"
	}
	if result == nil || len(result.IDs) == 0 {
		if len(item.ProviderIDs) == 0 {
			return models.MatchConfidenceUnidentified, "no_provider_match"
		}
		return models.MatchConfidenceLow, "provider_id_unconfirmed"
	}
	if matchingExplicitID(explicitIDs, result.IDs) {
		return models.MatchConfidenceHigh, "provider_id_confirmed"
	}
	if item.Type == models.ItemSeason || item.Type == models.ItemEpisode {
		if len(work.Hints.SeriesIDs) > 0 && item.SeasonNumber != nil &&
			(item.Type == models.ItemSeason || item.EpisodeNumber != nil) {
			return models.MatchConfidenceHigh, "series_episode_coordinates"
		}
	}

	actualTitle := result.Meta.Title
	expectedTitle := work.Hints.Title
	expectedYear := work.Hints.Year
	actualYear := result.Meta.Year
	if item.Type == models.ItemAlbum {
		actualTitle = firstNonEmpty(result.Meta.Album, result.Meta.Title)
		expectedTitle = firstNonEmpty(work.Hints.Album, work.Hints.Title)
		if normalizedMatchText(work.Hints.Artist) != "" && normalizedMatchText(work.Hints.Artist) != normalizedMatchText(firstNonEmpty(result.Meta.Artist, result.Meta.AlbumArtist)) {
			return models.MatchConfidenceLow, "artist_conflict"
		}
		if actualYear > 0 && expectedYear > 0 && actualYear != expectedYear {
			return models.MatchConfidenceLow, "year_conflict"
		}
		if sameMatchText(expectedTitle, actualTitle) && normalizedMatchText(work.Hints.Artist) != "" {
			return models.MatchConfidenceHigh, "album_artist_match"
		}
	} else {
		if actualYear > 0 && expectedYear > 0 && actualYear != expectedYear {
			return models.MatchConfidenceLow, "year_conflict"
		}
		if sameMatchText(expectedTitle, actualTitle) || sameMatchText(expectedTitle, result.Meta.OriginalTitle) {
			if expectedYear > 0 && actualYear > 0 {
				return models.MatchConfidenceHigh, "title_year_match"
			}
			return models.MatchConfidenceMedium, "title_match_year_unknown"
		}
	}
	if normalizedMatchText(expectedTitle) == "" || normalizedMatchText(actualTitle) == "" {
		return models.MatchConfidenceMedium, "insufficient_title_evidence"
	}
	return models.MatchConfidenceLow, "title_conflict"
}

func matchingExplicitID(expected, actual map[string]string) bool {
	for provider, id := range expected {
		if id != "" && actual[provider] == id {
			return true
		}
	}
	return false
}

func sameMatchText(left, right string) bool {
	left = normalizedMatchText(left)
	return left != "" && left == normalizedMatchText(right)
}

func normalizedMatchText(value string) string {
	var normalized strings.Builder
	space := true
	for _, char := range strings.ToLower(value) {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			normalized.WriteRune(char)
			space = false
		} else if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}

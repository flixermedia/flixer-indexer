package models

import (
	"fmt"
	"path/filepath"
	"strings"
)

// keyPriority orders provider ids by how stable and widely shared they are.
var keyPriority = []string{"tmdb", "tvdb", "imdb", "anilist", "anidb", "mal", "musicbrainz", "discogs", "audiodb"}

// ProviderKey returns the most authoritative provider id as "provider:id", or "".
func ProviderKey(ids map[string]string) string {
	for _, p := range keyPriority {
		if v := ids[p]; v != "" {
			return p + ":" + v
		}
	}
	return ""
}

// pathKey is the key of an unidentified item: its path relative to the library
// folder (root), so pointing the library at a different folder holding the same
// content keeps the same key. Without a root the absolute path is used.
func pathKey(root, path string) string {
	if root != "" {
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "path:" + filepath.ToSlash(rel)
		}
	}
	return "path:" + path
}

// AssignKeys fills ExternalKey, ParentExternalKey and ParentProviderIDs so flixer
// can keep identity and hierarchy stable when files or folders are renamed.
// Keys fall back to the path (relative to root, the item's library folder) when
// nothing has been identified.
func AssignKeys(item *Item, seriesIDs map[string]string, root string) {
	seriesKey := ProviderKey(seriesIDs)
	if item.Type == ItemSeries {
		seriesKey = ProviderKey(item.ProviderIDs)
	}
	seasonKey := func() string {
		if seriesKey == "" || item.SeasonNumber == nil {
			return ""
		}
		return fmt.Sprintf("%s:s%d", seriesKey, *item.SeasonNumber)
	}

	switch item.Type {
	case ItemSeries:
		item.ExternalKey = seriesKey
	case ItemSeason:
		item.ExternalKey = seasonKey()
		item.ParentExternalKey = seriesKey
	case ItemEpisode:
		if seasonKey() != "" && item.EpisodeNumber != nil {
			item.ExternalKey = fmt.Sprintf("%s:e%d", seasonKey(), *item.EpisodeNumber)
		}
		item.ParentExternalKey = seasonKey()
	default:
		item.ExternalKey = ProviderKey(item.ProviderIDs)
	}

	if item.Type == ItemSeason || item.Type == ItemEpisode {
		item.ParentProviderIDs = seriesIDs
	}
	if item.ExternalKey == "" {
		item.ExternalKey = pathKey(root, item.Path)
	}
	// Only series, seasons, episodes and tracks have a parent item; a video's
	// folder is not an item.
	hasParent := item.Type == ItemSeason || item.Type == ItemEpisode || item.Type == ItemTrack
	if item.ParentExternalKey == "" && item.ParentPath != "" && hasParent {
		item.ParentExternalKey = pathKey(root, item.ParentPath)
	}
}

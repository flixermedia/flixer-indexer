// Package models holds the structs shared between the indexer's packages and
// the JSON contract spoken with flixer.
package models

// Library types, mirroring flixer's media library types.
const (
	LibraryMovies     = "movies"
	LibraryTVShows    = "tvshows"
	LibraryMusic      = "music"
	LibraryHomeVideos = "homevideos"
	LibraryMixed      = "mixed"
)

// Item types produced by the indexer.
const (
	ItemMovie   = "movie"
	ItemSeries  = "series"
	ItemSeason  = "season"
	ItemEpisode = "episode"
	ItemAlbum   = "album"
	ItemTrack   = "track"
	ItemVideo   = "video"
)

// Image storage policies.
const (
	StorageAlongside = "alongside"
	StorageCache     = "cache"
)

// Image types.
const (
	ImagePoster   = "poster"
	ImageBackdrop = "backdrop"
	ImageLogo     = "logo"
	ImageBanner   = "banner"
	ImageThumb    = "thumb"
	ImageDisc     = "disc"
)

// Folder is one folder of a library. AbsolutePath is where the folder lives
// on disk for this process; RootID and Path identify it inside flixer.
type Folder struct {
	RootID       string `json:"rootId"`
	Path         string `json:"path"`
	AbsolutePath string `json:"absolutePath"`
}

// Library is a media library declared by an admin in flixer.
type Library struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	ImageStorage string   `json:"imageStorage"`
	Providers    []string `json:"providers,omitempty"`
	Folders      []Folder `json:"folders"`
}

// ProviderSetting is one provider's configuration from flixer.
type ProviderSetting struct {
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"apiKey,omitempty"`
}

// SubtitleSettings configures OpenSubtitles.
type SubtitleSettings struct {
	APIKey    string   `json:"apiKey,omitempty"`
	Username  string   `json:"username,omitempty"`
	Password  string   `json:"password,omitempty"`
	Languages []string `json:"languages,omitempty"`
	AutoFetch bool     `json:"autoFetch"`
}

// Settings is the dynamic configuration flixer owns.
type Settings struct {
	Providers   map[string]ProviderSetting `json:"providers"`
	Concurrency map[string]int             `json:"concurrency"`
	Language    string                     `json:"language,omitempty"`
	Subtitles   *SubtitleSettings          `json:"subtitles,omitempty"`
}

// Person is a cast or crew member.
type Person struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // actor, director, writer, producer, composer, ...
	Role  string `json:"role,omitempty"`
	Order int    `json:"order,omitempty"`
	Image string `json:"image,omitempty"`
}

// Metadata is the descriptive information about an item.
type Metadata struct {
	Title         string             `json:"title,omitempty"`
	OriginalTitle string             `json:"originalTitle,omitempty"`
	SortTitle     string             `json:"sortTitle,omitempty"`
	Overview      string             `json:"overview,omitempty"`
	Tagline       string             `json:"tagline,omitempty"`
	Year          int                `json:"year,omitempty"`
	PremiereDate  string             `json:"premiereDate,omitempty"`
	Runtime       int                `json:"runtime,omitempty"` // minutes
	Genres        []string           `json:"genres,omitempty"`
	Studios       []string           `json:"studios,omitempty"`
	Tags          []string           `json:"tags,omitempty"`
	Cast          []Person           `json:"cast,omitempty"`
	Crew          []Person           `json:"crew,omitempty"`
	Ratings       map[string]float64 `json:"ratings,omitempty"`
	ContentRating string             `json:"contentRating,omitempty"`
	Status        string             `json:"status,omitempty"`
	Language      string             `json:"language,omitempty"`
	Country       string             `json:"country,omitempty"`
	Collection    string             `json:"collection,omitempty"`
	Artist        string             `json:"artist,omitempty"`
	AlbumArtist   string             `json:"albumArtist,omitempty"`
	Album         string             `json:"album,omitempty"`
	Label         string             `json:"label,omitempty"`
	TrackNumber   int                `json:"trackNumber,omitempty"`
	DiscNumber    int                `json:"discNumber,omitempty"`
	TotalTracks   int                `json:"totalTracks,omitempty"`
	TotalSeasons  int                `json:"totalSeasons,omitempty"`
	TotalEpisodes int                `json:"totalEpisodes,omitempty"`
}

// Image is an artwork file. Path is set once the file is on disk; URL is a
// remote candidate that has not been downloaded yet.
type Image struct {
	Type     string  `json:"type"`
	Path     string  `json:"path,omitempty"`
	URL      string  `json:"url,omitempty"`
	Provider string  `json:"provider"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Language string  `json:"language,omitempty"`
	Score    float64 `json:"score,omitempty"`
	Season   *int    `json:"season,omitempty"`
}

// Technical is the ffprobe-derived description of a media file.
type Technical struct {
	Container         string    `json:"container,omitempty"`
	Duration          float64   `json:"duration,omitempty"` // seconds
	Bitrate           int64     `json:"bitrate,omitempty"`
	VideoCodec        string    `json:"videoCodec,omitempty"`
	AudioCodec        string    `json:"audioCodec,omitempty"`
	Width             int       `json:"width,omitempty"`
	Height            int       `json:"height,omitempty"`
	FrameRate         float64   `json:"frameRate,omitempty"`
	HDR               string    `json:"hdr,omitempty"`
	AudioChannels     int       `json:"audioChannels,omitempty"`
	BitDepth          int       `json:"bitDepth,omitempty"`
	SampleRate        int       `json:"sampleRate,omitempty"`
	AudioLanguages    []string  `json:"audioLanguages,omitempty"`
	SubtitleLanguages []string  `json:"subtitleLanguages,omitempty"`
	Chapters          []Chapter `json:"chapters,omitempty"`
}

// Chapter is a named span of a video, in seconds, from the container's chapter list.
type Chapter struct {
	Title string  `json:"title"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Item is what the indexer persists through flixer.
type Item struct {
	LibraryID  string `json:"libraryId"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	ParentPath string `json:"parentPath,omitempty"`
	// ExternalKey is a rename-proof identity (e.g. "tmdb:1234:s1:e2"), or
	// "path:<path>" when the item is unidentified. ParentExternalKey names the
	// parent the same way; ParentProviderIDs lets flixer match a series whose
	// own key differs (e.g. identified by a different provider).
	ExternalKey       string            `json:"externalKey"`
	ParentExternalKey string            `json:"parentExternalKey,omitempty"`
	ParentProviderIDs map[string]string `json:"parentProviderIds,omitempty"`
	RootID            string            `json:"rootId,omitempty"`
	RelativePath      string            `json:"relativePath,omitempty"`
	SeasonNumber      *int              `json:"seasonNumber,omitempty"`
	EpisodeNumber     *int              `json:"episodeNumber,omitempty"`
	EpisodeEnd        *int              `json:"episodeEndNumber,omitempty"`
	ProviderIDs       map[string]string `json:"providerIds"`
	Metadata          Metadata          `json:"metadata"`
	Images            []Image           `json:"images"`
	Technical         *Technical        `json:"technical,omitempty"`
	SizeBytes         int64             `json:"sizeBytes,omitempty"`
	ModifiedAt        string            `json:"modifiedAt,omitempty"`
	NFOPresent        bool              `json:"nfoPresent"`
	LocalImagesUsed   bool              `json:"localImagesUsed"`
}

// Hints are what the filesystem told us before any provider was asked.
type Hints struct {
	Title       string            `json:"title,omitempty"`
	Year        int               `json:"year,omitempty"`
	SeriesTitle string            `json:"seriesTitle,omitempty"`
	SeriesYear  int               `json:"seriesYear,omitempty"`
	SeriesPath  string            `json:"seriesPath,omitempty"`
	SeriesIDs   map[string]string `json:"seriesIds,omitempty"`
	Artist      string            `json:"artist,omitempty"`
	Album       string            `json:"album,omitempty"`
}

// Work is the payload carried between pipeline stages for a single item.
type Work struct {
	Item  Item  `json:"item"`
	Hints Hints `json:"hints"`
	Force bool  `json:"force,omitempty"`
	// NFOComplete means local metadata is enough to skip remote lookups.
	NFOComplete bool `json:"nfoComplete,omitempty"`
	// Untracked work (single-item refreshes) does not count toward scan progress.
	Untracked bool `json:"untracked,omitempty"`
}

// ScanRequest starts a scan of a library.
type ScanRequest struct {
	LibraryID     string `json:"libraryId"`
	Force         bool   `json:"force,omitempty"`
	TrickplayOnly bool   `json:"trickplayOnly,omitempty"`
}

// RefreshRequest re-runs the pipeline for one item.
type RefreshRequest struct {
	LibraryID string `json:"libraryId"`
	Path      string `json:"path"`
	ItemType  string `json:"itemType"`
}

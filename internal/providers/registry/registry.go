// Package registry builds the set of enabled metadata providers from flixer's
// settings and runs lookups across them in priority order.
package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
	"github.com/flixermedia/flixer-indexer/internal/providers/anidb"
	"github.com/flixermedia/flixer-indexer/internal/providers/anilist"
	"github.com/flixermedia/flixer-indexer/internal/providers/audiodb"
	"github.com/flixermedia/flixer-indexer/internal/providers/discogs"
	"github.com/flixermedia/flixer-indexer/internal/providers/fanart"
	"github.com/flixermedia/flixer-indexer/internal/providers/musicbrainz"
	"github.com/flixermedia/flixer-indexer/internal/providers/omdb"
	"github.com/flixermedia/flixer-indexer/internal/providers/tmdb"
	"github.com/flixermedia/flixer-indexer/internal/providers/tvdb"
)

// keyless providers work without credentials and are on unless disabled.
var keyless = map[string]bool{"musicbrainz": true, "coverartarchive": true, "anilist": true}

// Registry is an immutable set of providers; a new one is built whenever
// flixer's settings change.
type Registry struct {
	movie   []providers.MovieProvider
	series  []providers.SeriesProvider
	anime   []providers.SeriesProvider
	season  []providers.SeasonProvider
	episode []providers.EpisodeProvider
	album   []providers.AlbumProvider
	images  []providers.ImageProvider
	discogs *discogs.Provider
	stats    map[string]*httpx.Stats
	language string
	cache    *providers.Cache
}

type statsProvider interface{ Stats() *httpx.Stats }

// Build creates the providers enabled by settings, falling back to the
// environment-supplied keys where flixer has none.
func Build(settings models.Settings, envKeys map[string]string, userAgent, defaultLanguage string) *Registry {
	r := &Registry{stats: map[string]*httpx.Stats{}, language: settings.Language, cache: providers.NewCache(30 * time.Minute)}
	if r.language == "" {
		r.language = defaultLanguage
	}

	// resolve returns whether a provider is on and the key to use.
	resolve := func(name string) (string, bool) {
		setting, configured := settings.Providers[name]
		key := strings.TrimSpace(setting.APIKey)
		if key == "" {
			key = envKeys[name]
		}
		if configured && !setting.Enabled {
			return "", false
		}
		if keyless[name] {
			return "", true
		}
		return key, key != ""
	}
	track := func(name string, p any) {
		if s, ok := p.(statsProvider); ok {
			r.stats[name] = s.Stats()
		}
	}

	var tm *tmdb.Provider
	var tv *tvdb.Provider
	var om *omdb.Provider
	var fa *fanart.Provider
	if key, ok := resolve("tmdb"); ok {
		tm = tmdb.New(key, userAgent)
		track("tmdb", tm)
	}
	if key, ok := resolve("tvdb"); ok {
		tv = tvdb.New(key, userAgent)
		track("tvdb", tv)
	}
	if key, ok := resolve("omdb"); ok {
		om = omdb.New(key, userAgent)
		track("omdb", om)
	}
	if key, ok := resolve("fanart"); ok {
		fa = fanart.New(key, userAgent)
		track("fanart", fa)
	}

	if tm != nil {
		r.movie = append(r.movie, tm)
		r.series = append(r.series, tm)
		r.season = append(r.season, tm)
		r.episode = append(r.episode, tm)
	}
	if tv != nil {
		r.movie = append(r.movie, tv)
		r.series = append(r.series, tv)
		r.episode = append(r.episode, tv)
	}
	if om != nil {
		r.movie = append(r.movie, om)
		r.series = append(r.series, om)
		r.episode = append(r.episode, om)
	}
	if tm != nil {
		r.images = append(r.images, tm)
	}
	if fa != nil {
		r.images = append(r.images, fa)
	}

	if _, ok := resolve("anilist"); ok {
		al := anilist.New(userAgent)
		track("anilist", al)
		r.anime = append(r.anime, al)
	}
	if key, ok := resolve("anidb"); ok {
		ad := anidb.New(key, userAgent)
		track("anidb", ad)
		r.anime = append(r.anime, ad)
	}

	if _, ok := resolve("musicbrainz"); ok {
		mb := musicbrainz.New(userAgent)
		track("musicbrainz", mb)
		r.album = append(r.album, mb)
	}
	if key, ok := resolve("audiodb"); ok {
		ab := audiodb.New(key, userAgent)
		track("audiodb", ab)
		r.album = append(r.album, ab)
	}
	if key, ok := resolve("discogs"); ok {
		dg := discogs.New(key, userAgent)
		track("discogs", dg)
		r.album = append(r.album, dg)
		r.discogs = dg
	}
	if _, ok := resolve("coverartarchive"); ok {
		ca := musicbrainz.NewCoverArt(userAgent)
		track("coverartarchive", ca)
		r.images = append(r.images, ca)
	}
	return r
}

// Language is the metadata language providers are asked for.
func (r *Registry) Language() string { return r.language }

// Artist looks a music artist up on Discogs. It returns httpx.ErrNotFound when Discogs is not enabled.
func (r *Registry) Artist(ctx context.Context, name string) (*discogs.Artist, error) {
	if r.discogs == nil {
		return nil, httpx.ErrNotFound
	}
	return r.discogs.Artist(ctx, name)
}

// Stats returns per-provider request counters.
func (r *Registry) Stats() map[string]httpx.Snapshot {
	out := make(map[string]httpx.Snapshot, len(r.stats))
	for name, s := range r.stats {
		out[name] = s.Snapshot()
	}
	return out
}

// Names lists the enabled providers.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.stats))
	for name := range r.stats {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Restrict returns a registry limited to the named providers. An empty list
// means no restriction.
func (r *Registry) Restrict(names []string) *Registry {
	if len(names) == 0 {
		return r
	}
	allowed := map[string]bool{}
	for _, n := range names {
		allowed[strings.ToLower(n)] = true
	}
	// The Cover Art Archive is part of MusicBrainz.
	if allowed["musicbrainz"] {
		allowed["coverartarchive"] = true
	}
	out := *r
	out.movie = filter(r.movie, allowed)
	out.series = filter(r.series, allowed)
	out.anime = filter(r.anime, allowed)
	out.season = filter(r.season, allowed)
	out.episode = filter(r.episode, allowed)
	out.album = filter(r.album, allowed)
	out.images = filter(r.images, allowed)
	return &out
}

func filter[T providers.Provider](list []T, allowed map[string]bool) []T {
	var out []T
	for _, p := range list {
		if allowed[p.Name()] {
			out = append(out, p)
		}
	}
	return out
}

// HasMovie reports whether any provider can look up movies.
func (r *Registry) HasMovie() bool  { return len(r.movie) > 0 }
func (r *Registry) HasSeries() bool { return len(r.series)+len(r.anime) > 0 }
func (r *Registry) HasAlbum() bool  { return len(r.album) > 0 }

func (r *Registry) Movie(ctx context.Context, q providers.Query) (*providers.Result, error) {
	q = r.prepare(q)
	return r.cached(ctx, "movie", q, func() (*providers.Result, error) {
		return run(ctx, q, r.movie, func(p providers.MovieProvider, q providers.Query) (*providers.Result, error) {
			return p.Movie(ctx, q)
		})
	})
}

func (r *Registry) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	q = r.prepare(q)
	return r.cached(ctx, "series", q, func() (*providers.Result, error) {
		res, err := run(ctx, q, r.series, func(p providers.SeriesProvider, q providers.Query) (*providers.Result, error) {
			return p.Series(ctx, q)
		})
		if err != nil || len(r.anime) == 0 {
			return res, err
		}
		if res == nil && !looksLikeAnimeIDs(q.IDs) {
			return nil, nil
		}
		if res != nil && !looksLikeAnime(res) && !looksLikeAnimeIDs(q.IDs) {
			return res, nil
		}
		if res != nil {
			q.IDs = providers.MergeIDs(q.IDs, res.IDs)
			if q.Title == "" {
				q.Title = res.Meta.Title
			}
		}
		extra, err := run(ctx, q, r.anime, func(p providers.SeriesProvider, q providers.Query) (*providers.Result, error) {
			return p.Series(ctx, q)
		})
		if err != nil || extra == nil {
			return res, nil
		}
		if res == nil {
			// Without any mainstream hit, anime-only data still beats nothing.
			return extra, nil
		}
		providers.MergeMetadata(&res.Meta, extra.Meta)
		res.IDs = providers.MergeIDs(res.IDs, extra.IDs)
		res.Images = append(res.Images, extra.Images...)
		return res, nil
	})
}

func (r *Registry) Season(ctx context.Context, ids map[string]string, season int) (*providers.Result, error) {
	for _, p := range r.season {
		res, err := p.Season(ctx, ids, season, r.language)
		if err == nil && res != nil {
			return clone(res), nil
		}
		if err != nil && !errors.Is(err, httpx.ErrNotFound) {
			return nil, err
		}
	}
	return nil, nil
}

func (r *Registry) Episode(ctx context.Context, ids map[string]string, season, episode int) (*providers.Result, error) {
	key := fmt.Sprintf("episode|%v|%d|%d", ids, season, episode)
	return r.cached(ctx, key, providers.Query{}, func() (*providers.Result, error) {
		var merged *providers.Result
		var firstErr error
		for _, p := range r.episode {
			res, err := p.Episode(ctx, ids, season, episode, r.language)
			if err != nil {
				if !errors.Is(err, httpx.ErrNotFound) && firstErr == nil {
					firstErr = err
				}
				continue
			}
			merged = combine(merged, res)
		}
		if merged == nil {
			return nil, firstErr
		}
		return merged, nil
	})
}

func (r *Registry) Album(ctx context.Context, q providers.Query) (*providers.Result, error) {
	q = r.prepare(q)
	return r.cached(ctx, "album", q, func() (*providers.Result, error) {
		return run(ctx, q, r.album, func(p providers.AlbumProvider, q providers.Query) (*providers.Result, error) {
			return p.Album(ctx, q)
		})
	})
}

// Images asks the artwork providers for pictures of an identified item.
func (r *Registry) Images(ctx context.Context, kind string, ids map[string]string, season *int) []models.Image {
	var out []models.Image
	for _, p := range r.images {
		list, err := p.Images(ctx, kind, ids, season)
		if err == nil {
			out = append(out, list...)
		}
	}
	return out
}

func (r *Registry) prepare(q providers.Query) providers.Query {
	if q.Language == "" {
		q.Language = r.language
	}
	return q
}

func (r *Registry) cached(_ context.Context, kind string, q providers.Query, fn func() (*providers.Result, error)) (*providers.Result, error) {
	key := fmt.Sprintf("%s|%s|%d|%s|%v|%s", kind, strings.ToLower(q.Title), q.Year, strings.ToLower(q.Artist), q.IDs, q.Language)
	res, err := r.cache.Do(key, fn)
	if res == nil || err != nil {
		return nil, err
	}
	return clone(res), nil
}

// run asks each provider in order, feeding the ids found so far to the next.
// Not finding the item is not an error; only total failure is.
func run[T providers.Provider](ctx context.Context, q providers.Query, list []T, call func(T, providers.Query) (*providers.Result, error)) (*providers.Result, error) {
	q.IDs = providers.MergeIDs(nil, q.IDs)
	var merged *providers.Result
	var failure error
	for _, p := range list {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		res, err := call(p, q)
		if err != nil {
			if !errors.Is(err, httpx.ErrNotFound) && failure == nil {
				failure = err
			}
			continue
		}
		if res == nil {
			continue
		}
		merged = combine(merged, res)
		q.IDs = providers.MergeIDs(q.IDs, res.IDs)
		if q.Title == "" {
			q.Title = res.Meta.Title
		}
	}
	if merged == nil {
		return nil, failure
	}
	return merged, nil
}

func combine(dst, src *providers.Result) *providers.Result {
	if dst == nil {
		return clone(src)
	}
	providers.MergeMetadata(&dst.Meta, src.Meta)
	dst.IDs = providers.MergeIDs(dst.IDs, src.IDs)
	dst.Images = append(dst.Images, src.Images...)
	return dst
}

// clone copies the maps so results shared through the cache are never mutated.
func clone(r *providers.Result) *providers.Result {
	out := &providers.Result{
		IDs:    providers.MergeIDs(nil, r.IDs),
		Meta:   r.Meta,
		Images: append([]models.Image(nil), r.Images...),
	}
	if r.Meta.Ratings != nil {
		out.Meta.Ratings = make(map[string]float64, len(r.Meta.Ratings))
		for k, v := range r.Meta.Ratings {
			out.Meta.Ratings[k] = v
		}
	}
	return out
}

func looksLikeAnime(res *providers.Result) bool {
	for _, g := range res.Meta.Genres {
		if strings.EqualFold(g, "anime") || strings.EqualFold(g, "animation") {
			return res.Meta.Country == "JP" || res.Meta.Language == "ja"
		}
	}
	return false
}

func looksLikeAnimeIDs(ids map[string]string) bool {
	_, a := ids["anilist"]
	_, b := ids["anidb"]
	return a || b
}

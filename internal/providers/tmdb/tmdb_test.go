package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/providers"
)

func TestMovieLookup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "Inception" || r.URL.Query().Get("year") != "2010" {
			t.Errorf("unexpected search query %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("api_key") != "key" {
			t.Error("api key missing")
		}
		w.Write([]byte(`{"results":[{"id":27205}]}`))
	})
	mux.HandleFunc("/movie/27205", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"id":27205,"imdb_id":"tt1375666","title":"Inception","overview":"Dreams.","release_date":"2010-07-15",
			"runtime":148,"vote_average":8.4,"genres":[{"name":"Action"}],
			"production_companies":[{"name":"Warner Bros."}],
			"credits":{"cast":[{"name":"Leonardo DiCaprio","character":"Cobb","order":0}],
			           "crew":[{"name":"Christopher Nolan","job":"Director"},{"name":"Nobody","job":"Gaffer"}]},
			"images":{"posters":[{"file_path":"/p.jpg","width":2000,"height":3000,"iso_639_1":"en","vote_average":5}],
			          "backdrops":[{"file_path":"/b.jpg","width":3840,"height":2160,"iso_639_1":null,"vote_average":6}]},
			"release_dates":{"results":[{"iso_3166_1":"US","release_dates":[{"certification":"PG-13"}]}]}
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := New("key", "test").WithBase(server.URL)
	res, err := p.Movie(context.Background(), providers.Query{Title: "Inception", Year: 2010, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IDs["tmdb"] != "27205" || res.IDs["imdb"] != "tt1375666" {
		t.Errorf("ids = %v", res.IDs)
	}
	m := res.Meta
	if m.Title != "Inception" || m.Year != 2010 || m.Runtime != 148 || m.ContentRating != "PG-13" {
		t.Errorf("metadata = %+v", m)
	}
	if len(m.Cast) != 1 || m.Cast[0].Role != "Cobb" {
		t.Errorf("cast = %+v", m.Cast)
	}
	if len(m.Crew) != 1 || m.Crew[0].Type != "director" {
		t.Errorf("crew = %+v", m.Crew)
	}
	if len(res.Images) != 2 || res.Images[0].Type != "poster" || res.Images[1].URL != imageBase+"/b.jpg" {
		t.Errorf("images = %+v", res.Images)
	}
}

func TestResolveByIMDB(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/find/tt1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"movie_results":[{"id":42}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	id, err := New("key", "test").WithBase(server.URL).resolveID(context.Background(), "movie", providers.Query{IDs: map[string]string{"imdb": "tt1"}})
	if err != nil || id != "42" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
}

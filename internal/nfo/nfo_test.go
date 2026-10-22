package nfo

import "testing"

func TestParseMovie(t *testing.T) {
	data, err := Parse([]byte(`<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie>
  <title>Inception</title>
  <originaltitle>Inception</originaltitle>
  <year>2010</year>
  <plot>A thief who steals secrets.</plot>
  <runtime>148 min</runtime>
  <mpaa>PG-13</mpaa>
  <genre>Action</genre><genre>Sci-Fi</genre>
  <studio>Warner Bros.</studio>
  <director>Christopher Nolan</director>
  <set><name>Nolan Collection</name></set>
  <uniqueid type="imdb" default="true">tt1375666</uniqueid>
  <uniqueid type="tmdb">27205</uniqueid>
  <ratings><rating name="imdb" max="10"><value>8.8</value></rating></ratings>
  <actor><name>Leonardo DiCaprio</name><role>Cobb</role><order>0</order></actor>
</movie>
https://www.themoviedb.org/movie/27205`))
	if err != nil {
		t.Fatal(err)
	}
	m := data.Metadata
	if m.Title != "Inception" || m.Year != 2010 || m.Runtime != 148 || m.ContentRating != "PG-13" || m.Collection != "Nolan Collection" {
		t.Errorf("metadata = %+v", m)
	}
	if data.IDs["imdb"] != "tt1375666" || data.IDs["tmdb"] != "27205" {
		t.Errorf("ids = %v", data.IDs)
	}
	if m.Ratings["imdb"] != 8.8 || len(m.Cast) != 1 || m.Cast[0].Role != "Cobb" {
		t.Errorf("ratings/cast = %v / %+v", m.Ratings, m.Cast)
	}
	if len(m.Crew) != 1 || m.Crew[0].Type != "director" {
		t.Errorf("crew = %+v", m.Crew)
	}
	if !data.Complete() {
		t.Error("expected NFO to be complete")
	}
}

func TestParseEpisode(t *testing.T) {
	data, err := Parse([]byte("\xef\xbb\xbf<episodedetails><title>Pilot</title><season>1</season><episode>2</episode><aired>2008-01-20</aired></episodedetails>"))
	if err != nil {
		t.Fatal(err)
	}
	if data.Season == nil || *data.Season != 1 || data.Episode == nil || *data.Episode != 2 {
		t.Errorf("season/episode = %v/%v", data.Season, data.Episode)
	}
	if data.Metadata.Year != 2008 || data.Complete() {
		t.Errorf("metadata = %+v", data.Metadata)
	}
}

func TestParseGarbage(t *testing.T) {
	if _, err := Parse([]byte("not xml at all")); err == nil {
		t.Error("expected an error")
	}
}

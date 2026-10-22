package discogs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtistUsesExactMatchAndCleansMarkup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/database/search":
			w.Write([]byte(`{"results":[{"id":1,"title":"Zach Bryant"},{"id":2,"title":"Zach Bryan"}]}`))
		case r.URL.Path == "/artists/2":
			w.Write([]byte(`{"name":"Zach Bryan","realname":"Zachary Bryan","uri":"https://www.discogs.com/artist/2","profile":"Singer. Brother of [a=Someone] and [a123]. See [url=https://x.test]this[/url] [b]now[/b].","images":[{"type":"secondary","uri":"https://i.discogs.com/b.jpeg"},{"type":"primary","uri":"https://i.discogs.com/a.jpeg"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a, err := New("token", "test").WithBase(srv.URL).Artist(context.Background(), "zach bryan")
	if err != nil {
		t.Fatal(err)
	}
	if a.ImageURL != "https://i.discogs.com/a.jpeg" || a.RealName != "Zachary Bryan" {
		t.Fatalf("artist = %+v", a)
	}
	if strings.ContainsAny(a.Profile, "[]") || !strings.Contains(a.Profile, "Brother of Someone and . See this now.") {
		t.Fatalf("profile = %q", a.Profile)
	}
	if _, err := New("token", "test").WithBase(srv.URL).Artist(context.Background(), "Nobody"); err == nil {
		t.Fatal("expected not found")
	}
}

package providers

import (
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

func TestMergeMetadataPersonImageFallback(t *testing.T) {
	dst := models.Metadata{Cast: []models.Person{{Name: "Example Actor", Role: "Lead", Image: "/old/actor.jpg"}}}
	src := models.Metadata{Cast: []models.Person{{Name: "example actor", Role: "Different", Image: "https://image.tmdb.org/t/p/original/actor.jpg"}, {Name: "Other", Image: "https://example.com/image.jpg"}}}
	MergeMetadata(&dst, src)
	person := dst.Cast[0]
	if len(dst.Cast) != 1 || person.Role != "Lead" || person.Image != "/old/actor.jpg" || person.FallbackImage != src.Cast[0].Image {
		t.Fatalf("unexpected merged cast: %+v", dst.Cast)
	}
	if AllowedPersonImage("https://image.tmdb.org.evil.example/photo.jpg") || AllowedPersonImage("http://image.tmdb.org/photo.jpg") {
		t.Fatal("accepted an unsafe photo URL")
	}
}

package mediaid

import "testing"

func TestKeyIsStableAndRequiresPathIdentity(t *testing.T) {
	first, err := Key("root", "folder/video.mkv")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Key("root", "folder/video.mkv")
	if err != nil || first != second || len(first) != 32 {
		t.Fatalf("keys = %q, %q, %v", first, second, err)
	}
	if _, err := Key("", "folder/video.mkv"); err == nil {
		t.Fatal("missing root id should be rejected")
	}
	if _, err := Key("root", ""); err == nil {
		t.Fatal("missing relative path should be rejected")
	}
}

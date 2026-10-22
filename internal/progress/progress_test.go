package progress

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	if s, _ := tr.Get(ctx, "lib"); s != nil {
		t.Fatal("expected no scan")
	}
	_ = tr.Start(ctx, "lib")
	_ = tr.Discovered(ctx, "lib")
	_ = tr.Discovered(ctx, "lib")
	_ = tr.Done(ctx, "lib")

	s, _ := tr.Get(ctx, "lib")
	if s.State != StateScanning || s.Discovered != 2 || s.Done != 1 {
		t.Fatalf("scan = %+v", s)
	}

	_ = tr.WalkDone(ctx, "lib")
	if s, _ = tr.Get(ctx, "lib"); s.State != StateIndexing {
		t.Fatalf("scan = %+v", s)
	}
	_ = tr.Failed(ctx, "lib")
	s, _ = tr.Get(ctx, "lib")
	if s.State != StateComplete || s.Failed != 1 || s.FinishedAt == "" {
		t.Fatalf("scan = %+v", s)
	}

	all, err := tr.All(ctx)
	if err != nil || len(all) != 1 || all[0].LibraryID != "lib" {
		t.Fatalf("all = %+v, %v", all, err)
	}
}

func TestEmptyLibraryCompletes(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	_ = tr.Start(ctx, "x")
	_ = tr.WalkDone(ctx, "x")
	if s, _ := tr.Get(ctx, "x"); s.State != StateComplete {
		t.Fatalf("scan = %+v", s)
	}
}

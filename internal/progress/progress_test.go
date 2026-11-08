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
	_ = tr.Start(ctx, "lib", "run", true)
	_ = tr.Discovered(ctx, "lib", "run")
	_ = tr.Discovered(ctx, "lib", "run")
	_ = tr.Done(ctx, "lib", "run", "one")

	s, _ := tr.Get(ctx, "lib")
	if s.State != StateScanning || s.Discovered != 2 || s.Done != 1 {
		t.Fatalf("scan = %+v", s)
	}

	_ = tr.WalkDone(ctx, "lib", "run")
	if s, _ = tr.Get(ctx, "lib"); s.State != StateIndexing {
		t.Fatalf("scan = %+v", s)
	}
	_ = tr.Failed(ctx, "lib", "run", "two")
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
	_ = tr.Start(ctx, "x", "run", true)
	_ = tr.WalkDone(ctx, "x", "run")
	if s, _ := tr.Get(ctx, "x"); s.State != StateComplete {
		t.Fatalf("scan = %+v", s)
	}
	if _, ok, err := tr.ClaimPrune(ctx, "x", "run"); err != nil || ok {
		t.Fatalf("empty scan claimed prune: %v, %v", ok, err)
	}
}

func TestProgressIsolationAndPrune(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	if err := tr.Start(ctx, "lib", "old", true); err != nil {
		t.Fatal(err)
	}
	_ = tr.Discovered(ctx, "lib", "old")
	_ = tr.WalkDone(ctx, "lib", "old")
	if err := tr.Start(ctx, "lib", "one", true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := tr.ClaimPrune(ctx, "lib", "old"); err != nil || ok {
		t.Fatalf("superseded scan claimed prune: %v, %v", ok, err)
	}
	_ = tr.Discovered(ctx, "lib", "one")
	_ = tr.Discovered(ctx, "lib", "one")
	_ = tr.WalkDone(ctx, "lib", "one")
	_ = tr.Done(ctx, "lib", "old", "other")
	_ = tr.Done(ctx, "lib", "", "legacy")
	_ = tr.Done(ctx, "lib", "one", "first")
	_ = tr.Done(ctx, "lib", "one", "first")
	scan, err := tr.Get(ctx, "lib")
	if err != nil || scan.State != StateIndexing || scan.Done != 1 {
		t.Fatalf("scan after stale and duplicate completions = %+v, %v", scan, err)
	}
	if _, ok, err := tr.ClaimPrune(ctx, "lib", "one"); err != nil || ok {
		t.Fatalf("incomplete scan claimed prune: %v, %v", ok, err)
	}
	_ = tr.Done(ctx, "lib", "one", "second")
	if _, ok, err := tr.ClaimPrune(ctx, "lib", "one"); err != nil || !ok {
		t.Fatalf("completed scan did not claim prune: %v, %v", ok, err)
	}
	if err := tr.Start(ctx, "lib", "two", true); err == nil {
		t.Fatal("scan started while cleanup was in flight")
	}
	if err := tr.FinishPrune(ctx, "lib", "one"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Start(ctx, "lib", "two", false); err != nil {
		t.Fatal(err)
	}
	_ = tr.Done(ctx, "lib", "one", "late")
	_ = tr.WalkDone(ctx, "lib", "two")
	scan, err = tr.Get(ctx, "lib")
	if err != nil || scan.Done != 0 || scan.State != StateComplete {
		t.Fatalf("new scan affected by old work: %+v, %v", scan, err)
	}
	if _, ok, err := tr.ClaimPrune(ctx, "lib", "two"); err != nil || ok {
		t.Fatalf("targeted scan claimed prune: %v, %v", ok, err)
	}
}

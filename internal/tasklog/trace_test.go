package tasklog

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestTaskTraceparentIsStableAndRequestSpansAreDistinct(t *testing.T) {
	if Traceparent(context.Background()) != "" {
		t.Fatal("non-task context produced a trace")
	}

	ctx := WithTask(context.Background(), "scan", "task-1")
	first, second := Traceparent(ctx), Traceparent(ctx)
	pattern := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)
	a, b := pattern.FindStringSubmatch(first), pattern.FindStringSubmatch(second)
	if len(a) != 3 || len(b) != 3 || a[1] != b[1] || a[2] == b[2] ||
		a[1] == strings.Repeat("0", 32) || a[2] == strings.Repeat("0", 16) {
		t.Fatalf("invalid task trace headers: %q %q", first, second)
	}
	retry := pattern.FindStringSubmatch(Traceparent(WithTask(context.Background(), "scan", "task-1")))
	other := pattern.FindStringSubmatch(Traceparent(WithTask(context.Background(), "probe", "task-1")))
	if retry[1] != a[1] || other[1] == a[1] {
		t.Fatalf("task traces were not scoped to queue and ID: %q %q", retry[1], other[1])
	}
}

func TestTaskTraceparentConcurrentSpans(t *testing.T) {
	ctx := WithTask(context.Background(), "scan", "concurrent-task")
	var headers sync.Map
	var workers sync.WaitGroup
	for range 1000 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			header := Traceparent(ctx)
			if _, duplicate := headers.LoadOrStore(header, true); duplicate {
				t.Errorf("duplicate span header: %s", header)
			}
		}()
	}
	workers.Wait()
}

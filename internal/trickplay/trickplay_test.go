package trickplay

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/probe"
)

type fakeProber struct {
	duration float64
}

func (p fakeProber) Probe(context.Context, string) (*probe.Result, error) {
	return &probe.Result{Technical: models.Technical{VideoCodec: "h264", Width: 1920, Height: 1080, Duration: p.duration}}, nil
}

func TestKeyAndFFmpegArgs(t *testing.T) {
	if got := Key("root", "folder/movie.mkv"); got != "3cd1d927d86b484d09ba44c652e3a40d" {
		t.Fatalf("key = %q", got)
	}
	args := ffmpegArgs("/media/movie.mkv", "/cache/%d.jpg", 10, 320)
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-map 0:v:0", "-an", "-sn", "fps=1/10,scale=320:-2,showinfo,tile=10x10", "-q:v 5", "-threads 2", "/cache/%d.jpg"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("ffmpeg args %q missing %q", joined, expected)
		}
	}
}

func TestGenerateWritesAtomicPublicIndexAndSkipsMatchingSource(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "video.mp4")
	if err := os.WriteFile(mediaPath, []byte("video source"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache")
	runs := 0
	generator := &Generator{
		Directory: cache, FFmpeg: "unused", Interval: 10, Width: 80, Prober: fakeProber{duration: 95},
		Runner: func(_ context.Context, _ string, args []string, stderr io.Writer) error {
			runs++
			for frame := 0; frame < 10; frame++ {
				_, _ = fmt.Fprintf(stderr, "[Parsed_showinfo_2 @ 0x1] n: %d pts: %d\n", frame, frame)
			}
			pattern := args[len(args)-1]
			file, err := os.Create(strings.Replace(pattern, "%d", "0", 1))
			if err != nil {
				return err
			}
			defer file.Close()
			return jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 800, 20)), nil)
		},
	}
	task := Task{LibraryID: "lib", Path: mediaPath, RootID: "root", RelativePath: "folder/video.mp4"}
	if err := generator.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("runner calls = %d", runs)
	}
	keyDir := filepath.Join(cache, Key(task.RootID, task.RelativePath))
	data, err := os.ReadFile(filepath.Join(keyDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.Version != 1 || index.IntervalMs != 10000 || index.ThumbWidth != 80 || index.ThumbHeight != 2 || index.Columns != 10 || index.Rows != 10 || index.Count != 10 || index.Sheets != 1 || index.SourceSize != int64(len("video source")) || index.SourceModifiedAt == "" {
		t.Fatalf("unexpected index: %+v", index)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(keyDir, "index.json"), 0o644}, {filepath.Join(keyDir, "0.jpg"), 0o644}} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != item.mode {
			t.Errorf("%s mode = %o, want %o", item.path, info.Mode().Perm(), item.mode)
		}
	}
	for _, directory := range []string{cache, keyDir} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("directory %s mode = %o, want 755", directory, info.Mode().Perm())
		}
	}
	if err := generator.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("matching source reran ffmpeg: %d calls", runs)
	}
}

func TestGenerateSkipsShortAndEmptyUnknownDuration(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "video.mp4")
	if err := os.WriteFile(mediaPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := 0
	generator := &Generator{
		Directory: filepath.Join(root, "cache"), Interval: 10, Width: 320,
		Prober: fakeProber{duration: 20},
		Runner: func(context.Context, string, []string, io.Writer) error { runs++; return nil },
	}
	task := Task{Path: mediaPath, RootID: "root", RelativePath: "video.mp4"}
	if err := generator.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("short file invoked ffmpeg %d times", runs)
	}
	generator.Prober = nil
	if err := generator.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("empty unknown-duration file invoked ffmpeg %d times", runs)
	}
	if _, err := os.Stat(filepath.Join(generator.Directory, Key(task.RootID, task.RelativePath), "index.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected index after empty ffmpeg output: %v", err)
	}
}

func TestFrameCounterCountsShowinfoFrames(t *testing.T) {
	counter := &frameCounter{}
	_, _ = counter.Write([]byte("[Parsed_showinfo_2 @ 0x1] n:   0 pts: 0\n[Parsed_showinfo_2 @ 0x1] n:   1 pts: 1\n"))
	if counter.count != 2 {
		t.Fatalf("count = %d, want 2", counter.count)
	}
}

func TestGenerateUnknownDurationCountsFramesAndSkipsNonVideo(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "video.mp4")
	if err := os.WriteFile(mediaPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := 0
	generator := &Generator{
		Directory: filepath.Join(root, "cache"), FFmpeg: "unused", Interval: 10, Width: 80,
		Runner: func(_ context.Context, _ string, args []string, stderr io.Writer) error {
			runs++
			_, _ = io.WriteString(stderr, "[Parsed_showinfo_2 @ 0x1] n:   0 pts: 0\n[Parsed_showinfo_2 @ 0x1] n:   1 pts: 1\n[Parsed_showinfo_2 @ 0x1] n:   2 pts: 2\n")
			file, err := os.Create(strings.Replace(args[len(args)-1], "%d", "0", 1))
			if err != nil {
				return err
			}
			defer file.Close()
			return jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 800, 20)), nil)
		},
	}
	task := Task{Path: mediaPath, RootID: "root", RelativePath: "video.mp4"}
	if err := generator.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(generator.Directory, Key(task.RootID, task.RelativePath), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.Count != 3 || runs != 1 {
		t.Fatalf("unknown-duration index count=%d runner calls=%d", index.Count, runs)
	}

	audio := &Generator{Directory: filepath.Join(root, "audio-cache"), Interval: 10, Width: 320, Prober: noVideoProber{}, Runner: func(context.Context, string, []string, io.Writer) error { runs++; return nil }}
	if err := audio.Generate(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("non-video item invoked ffmpeg, calls=%d", runs)
	}
}

type noVideoProber struct{}

func (noVideoProber) Probe(context.Context, string) (*probe.Result, error) {
	return &probe.Result{Technical: models.Technical{AudioCodec: "aac", Duration: 3600}}, nil
}

func TestFrameCounterForwardsOutputWithoutPerFrameLines(t *testing.T) {
	var lines []string
	c := &frameCounter{sink: func(l string) { lines = append(lines, l) }}
	_, _ = c.Write([]byte("Input #0, matroska\n"))
	for i := 0; i < 120; i++ {
		_, _ = c.Write([]byte("[Parsed_showinfo_2 @ 0x1] n:   0 pts:1\n"))
	}
	_, _ = c.Write([]byte("[Parsed_showinfo_2 @ 0x1]   color_range:pc color_space:bt709\n"))
	_, _ = c.Write([]byte("error: boom"))
	c.flush()
	if c.count != 120 {
		t.Fatalf("count = %d", c.count)
	}
	want := []string{"Input #0, matroska", "extracted 50 frames", "extracted 100 frames", "error: boom", "extracted 120 frames"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", lines)
	}
}

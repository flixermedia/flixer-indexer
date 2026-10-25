package trickplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/probe"
)

const (
	Columns = 10
	Rows    = 10
)

type Prober interface {
	Probe(context.Context, string) (*probe.Result, error)
}

type Generator struct {
	Directory string
	FFmpeg    string
	Interval  int
	Width     int
	Prober    Prober
	Runner    func(context.Context, string, []string, io.Writer) error
}

type Task struct {
	LibraryID    string `json:"libraryId"`
	Path         string `json:"path"`
	RootID       string `json:"rootId"`
	RelativePath string `json:"relativePath"`
}

type Index struct {
	Version          int    `json:"version"`
	IntervalMs       int    `json:"intervalMs"`
	ThumbWidth       int    `json:"thumbWidth"`
	ThumbHeight      int    `json:"thumbHeight"`
	Columns          int    `json:"columns"`
	Rows             int    `json:"rows"`
	Count            int    `json:"count"`
	Sheets           int    `json:"sheets"`
	SourceSize       int64  `json:"sourceSize"`
	SourceModifiedAt string `json:"sourceModifiedAt"`
}

func Key(rootID, relativePath string) string {
	sum := sha256.Sum256([]byte(rootID + "\n" + relativePath))
	return hex.EncodeToString(sum[:])[:32]
}

func (g *Generator) Generate(ctx context.Context, task Task) error {
	if task.RootID == "" || task.RelativePath == "" || task.Path == "" {
		return errors.New("invalid trickplay task")
	}
	info, err := os.Stat(task.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	modified := info.ModTime().UTC().Format(time.RFC3339)
	key := Key(task.RootID, task.RelativePath)
	destination := filepath.Join(g.Directory, key)
	if current, err := os.ReadFile(filepath.Join(destination, "index.json")); err == nil {
		var index Index
		if json.Unmarshal(current, &index) == nil && index.SourceSize == info.Size() && index.SourceModifiedAt == modified {
			return nil
		}
	}

	var duration float64
	durationKnown := false
	if g.Prober != nil {
		if result, probeErr := g.Prober.Probe(ctx, task.Path); probeErr == nil && result != nil {
			technical := result.Technical
			if technical.VideoCodec == "" || technical.Width <= 0 || technical.Height <= 0 {
				return nil
			}
			duration = technical.Duration
			durationKnown = duration > 0 && !math.IsInf(duration, 0) && !math.IsNaN(duration)
		}
	}
	if durationKnown && duration < 30 {
		return nil
	}
	if g.Interval < 2 || g.Interval > 60 || g.Width < 80 || g.Width > 640 {
		return errors.New("invalid trickplay settings")
	}
	if err := os.MkdirAll(g.Directory, 0o755); err != nil {
		return fmt.Errorf("create trickplay directory: %w", err)
	}
	if err := os.Chmod(g.Directory, 0o755); err != nil {
		return fmt.Errorf("set trickplay directory permissions: %w", err)
	}
	workDir, err := os.MkdirTemp(g.Directory, "."+key+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary trickplay directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	if err := os.Chmod(workDir, 0o755); err != nil {
		return fmt.Errorf("set trickplay directory permissions: %w", err)
	}

	pattern := filepath.Join(workDir, "%d.jpg")
	counter := &frameCounter{}
	timeout := generationTimeout(duration)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := ffmpegArgs(task.Path, pattern, g.Interval, g.Width)
	runErr := error(nil)
	if g.Runner != nil {
		runErr = g.Runner(runCtx, g.FFmpeg, args, counter)
	} else {
		cmd := exec.CommandContext(runCtx, g.FFmpeg, args...)
		cmd.Stdout = nil
		cmd.Stderr = counter
		runErr = cmd.Run()
	}
	if runErr != nil {
		if runCtx.Err() != nil {
			return errors.New("ffmpeg timed out while generating trickplay")
		}
		return errors.New("ffmpeg failed while generating trickplay")
	}

	sheets, err := sheetFiles(workDir)
	if err != nil {
		return fmt.Errorf("inspect generated trickplay sheets: %w", err)
	}
	if len(sheets) == 0 {
		return nil
	}
	sheet, err := os.Open(filepath.Join(workDir, sheets[0]))
	if err != nil {
		return errors.New("generated trickplay sheet is not readable")
	}
	config, decodeErr := jpeg.DecodeConfig(sheet)
	_ = sheet.Close()
	if decodeErr != nil {
		return errors.New("generated trickplay sheet is not a valid JPEG")
	}
	count := counter.count
	maxCount := len(sheets) * Columns * Rows
	if count <= 0 {
		return nil
	}
	if count > maxCount {
		count = maxCount
	}
	idx := Index{
		Version: 1, IntervalMs: g.Interval * 1000,
		ThumbWidth: g.Width, ThumbHeight: config.Height / Rows,
		Columns: Columns, Rows: Rows, Count: count, Sheets: len(sheets),
		SourceSize: info.Size(), SourceModifiedAt: modified,
	}
	indexBytes, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("encode trickplay index: %w", err)
	}
	for _, name := range sheets {
		if err := os.Chmod(filepath.Join(workDir, name), 0o644); err != nil {
			return fmt.Errorf("set trickplay sheet permissions: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "index.json"), indexBytes, 0o644); err != nil {
		return fmt.Errorf("write trickplay index: %w", err)
	}
	if err := os.Chmod(filepath.Join(workDir, "index.json"), 0o644); err != nil {
		return fmt.Errorf("set trickplay index permissions: %w", err)
	}

	if err := os.RemoveAll(destination); err != nil {
		return fmt.Errorf("replace old trickplay sheets: %w", err)
	}
	if err := os.Rename(workDir, destination); err != nil {
		return fmt.Errorf("publish trickplay sheets: %w", err)
	}
	return nil
}

func ffmpegArgs(input, output string, interval, width int) []string {
	filter := fmt.Sprintf("fps=1/%d,scale=%d:-2,showinfo,tile=10x10", interval, width)
	return []string{"-hide_banner", "-loglevel", "info", "-nostats", "-nostdin", "-i", input, "-map", "0:v:0", "-an", "-sn", "-vf", filter, "-q:v", "5", "-threads", "2", "-start_number", "0", "-f", "image2", output}
}

func generationTimeout(duration float64) time.Duration {
	if duration > float64((3*time.Hour-10*time.Minute)/(2*time.Second)) {
		return 3 * time.Hour
	}
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 10 * time.Minute
	}
	return 10*time.Minute + time.Duration(duration*2)*time.Second
}

var sheetName = regexp.MustCompile(`^(0|[1-9][0-9]*)\.jpg$`)

func sheetFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !sheetName.MatchString(entry.Name()) {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Slice(files, func(i, j int) bool {
		left, _ := strconv.Atoi(strings.TrimSuffix(files[i], ".jpg"))
		right, _ := strconv.Atoi(strings.TrimSuffix(files[j], ".jpg"))
		return left < right
	})
	for i, name := range files {
		if name != strconv.Itoa(i)+".jpg" {
			return nil, errors.New("sheet numbering is not contiguous")
		}
	}
	return files, nil
}

type frameCounter struct {
	count int
	line  []byte
}

func (c *frameCounter) Write(data []byte) (int, error) {
	for _, b := range data {
		if b == '\n' {
			line := string(c.line)
			if strings.Contains(line, "] n:") {
				c.count++
			}
			c.line = c.line[:0]
		} else if len(c.line) < 4096 {
			c.line = append(c.line, b)
		}
	}
	return len(data), nil
}

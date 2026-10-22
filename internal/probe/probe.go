// Package probe wraps ffprobe to read technical details and embedded tags.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

// Result is the parsed ffprobe output.
type Result struct {
	Technical models.Technical
	// Tags are the container's tags with lower-cased keys.
	Tags map[string]string
	// HasEmbeddedArt is true when the file carries an attached picture.
	HasEmbeddedArt bool
}

// Prober runs ffprobe.
type Prober struct {
	Path    string
	Timeout time.Duration
}

func New(path string) *Prober { return &Prober{Path: path, Timeout: 2 * time.Minute} }

// Probe inspects a media file.
func (p *Prober) Probe(ctx context.Context, file string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Path, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", file)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ffprobe timed out on %s", file)
		}
		return nil, fmt.Errorf("ffprobe failed on %s: %w: %s", file, err, strings.TrimSpace(stderr.String()))
	}
	return Parse(stdout.Bytes())
}

type output struct {
	Format struct {
		Name     string            `json:"format_name"`
		Duration string            `json:"duration"`
		BitRate  string            `json:"bit_rate"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Type          string            `json:"codec_type"`
		Codec         string            `json:"codec_name"`
		Width         int               `json:"width"`
		Height        int               `json:"height"`
		FrameRate     string            `json:"avg_frame_rate"`
		RFrameRate    string            `json:"r_frame_rate"`
		Channels      int               `json:"channels"`
		SampleRate    string            `json:"sample_rate"`
		BitsRaw       string            `json:"bits_per_raw_sample"`
		BitsPerSample int               `json:"bits_per_sample"`
		PixFmt        string            `json:"pix_fmt"`
		ColorTransfer string            `json:"color_transfer"`
		Tags          map[string]string `json:"tags"`
		Disposition   map[string]int    `json:"disposition"`
		SideDataList  []struct {
			Type string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

var pixFmtDepth = regexp.MustCompile(`(?:p|^)(\d{2})(?:le|be)?$`)

// Parse converts ffprobe JSON into a Result.
func Parse(data []byte) (*Result, error) {
	var out output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	res := &Result{Tags: map[string]string{}}
	t := &res.Technical
	t.Container = strings.SplitN(out.Format.Name, ",", 2)[0]
	t.Duration, _ = strconv.ParseFloat(out.Format.Duration, 64)
	t.Bitrate, _ = strconv.ParseInt(out.Format.BitRate, 10, 64)
	for k, v := range out.Format.Tags {
		res.Tags[strings.ToLower(k)] = v
	}

	seenVideo, seenAudio := false, false
	for _, s := range out.Streams {
		switch s.Type {
		case "video":
			if s.Disposition["attached_pic"] == 1 {
				res.HasEmbeddedArt = true
				continue
			}
			if seenVideo {
				continue
			}
			seenVideo = true
			t.VideoCodec, t.Width, t.Height = s.Codec, s.Width, s.Height
			t.FrameRate = parseRate(s.FrameRate)
			if t.FrameRate == 0 {
				t.FrameRate = parseRate(s.RFrameRate)
			}
			t.HDR = hdrKind(s.ColorTransfer, s.SideDataList)
			t.BitDepth = bitDepth(s.BitsRaw, s.BitsPerSample, s.PixFmt)
		case "audio":
			if lang := strings.ToLower(s.Tags["language"]); lang != "" && lang != "und" {
				t.AudioLanguages = appendUnique(t.AudioLanguages, lang)
			}
			if seenAudio {
				continue
			}
			seenAudio = true
			t.AudioCodec, t.AudioChannels = s.Codec, s.Channels
			t.SampleRate, _ = strconv.Atoi(s.SampleRate)
			if !seenVideo && t.BitDepth == 0 {
				t.BitDepth = bitDepth(s.BitsRaw, s.BitsPerSample, "")
			}
		case "subtitle":
			if lang := strings.ToLower(s.Tags["language"]); lang != "" && lang != "und" {
				t.SubtitleLanguages = appendUnique(t.SubtitleLanguages, lang)
			}
		}
	}
	return res, nil
}

func hdrKind(transfer string, sideData []struct {
	Type string `json:"side_data_type"`
}) string {
	dolby, plus := false, false
	for _, sd := range sideData {
		lower := strings.ToLower(sd.Type)
		if strings.Contains(lower, "dovi") || strings.Contains(lower, "dolby vision") {
			dolby = true
		}
		if strings.Contains(lower, "2094-40") {
			plus = true
		}
	}
	switch {
	case dolby:
		return "dolbyvision"
	case plus:
		return "hdr10plus"
	case transfer == "smpte2084":
		return "hdr10"
	case transfer == "arib-std-b67":
		return "hlg"
	}
	return ""
}

func bitDepth(raw string, perSample int, pixFmt string) int {
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return n
	}
	if perSample > 0 {
		return perSample
	}
	if m := pixFmtDepth.FindStringSubmatch(pixFmt); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n >= 8 && n <= 16 {
			return n
		}
	}
	return 0
}

func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, _ := strconv.ParseFloat(num, 64)
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return float64(int(n/d*100+0.5)) / 100
}

func appendUnique(list []string, value string) []string {
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}

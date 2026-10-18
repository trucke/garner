// Package inspect reads what a downloaded file actually holds and compares
// it with what its release promised.
package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trucke/garner/internal/api"
	"github.com/trucke/garner/internal/release"
)

// probeTimeout bounds one FFprobe run; files are untrusted and may sit on
// a network share.
const probeTimeout = 2 * time.Minute

// Probe is what FFprobe found in a file.
type Probe struct {
	Duration  time.Duration
	Video     []Video
	Audio     []Track
	Subtitles []Track
}

// Video is a video stream.
type Video struct {
	Codec         string
	Width, Height int
}

// Track is an audio or subtitle stream. Language is the raw tag, such as
// "ger"; tags are evidence of the spoken language, not proof.
type Track struct {
	Codec    string
	Language string
	Title    string
}

// ErrUnreadable means FFprobe could not make sense of the file: the file,
// not the setup, is at fault.
var ErrUnreadable = errors.New("ffprobe could not read the file")

// Prober runs FFprobe.
type Prober struct{ bin string }

// NewProber finds the ffprobe program, a name in PATH or a path.
func NewProber(bin string) (*Prober, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("ffprobe not found (%s): install FFmpeg or set ffprobe in the config", bin)
	}
	return &Prober{bin: p}, nil
}

// Probe reads a file's streams. FFprobe runs without Garner's environment,
// so it never sees API keys, and may only open local files, so a crafted
// file cannot make it fetch anything.
func (p *Prober) Probe(ctx context.Context, file string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin, "-v", "error", "-protocol_whitelist", "file",
		"-print_format", "json", "-show_format", "-show_streams", "file:"+file)
	cmd.Env = []string{}
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return Probe{}, fmt.Errorf("ffprobe took longer than %s", probeTimeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return Probe{}, fmt.Errorf("%w: %s", ErrUnreadable, strings.TrimSpace(string(exit.Stderr)))
		}
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}
	return Parse(out)
}

// Parse reads FFprobe's JSON output.
func Parse(out []byte) (Probe, error) {
	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string            `json:"codec_type"`
			CodecName string            `json:"codec_name"`
			Width     int               `json:"width"`
			Height    int               `json:"height"`
			Tags      map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Probe{}, fmt.Errorf("unreadable ffprobe output: %w", err)
	}
	var p Probe
	if secs, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
		p.Duration = time.Duration(secs * float64(time.Second))
	}
	for _, s := range raw.Streams {
		t := Track{Codec: s.CodecName, Language: s.Tags["language"], Title: s.Tags["title"]}
		switch s.CodecType {
		case "video":
			// Cover art is stored as a video stream too.
			if s.CodecName != "mjpeg" && s.CodecName != "png" {
				p.Video = append(p.Video, Video{Codec: s.CodecName, Width: s.Width, Height: s.Height})
			}
		case "audio":
			p.Audio = append(p.Audio, t)
		case "subtitle":
			p.Subtitles = append(p.Subtitles, t)
		}
	}
	return p, nil
}

// Expect is what the release promised for one file.
type Expect struct {
	// Languages are acceptable audio languages; one must be present.
	Languages []release.Language
	// Strict makes a missing language a problem; otherwise, as for a
	// release chosen by hand, it is only noted.
	Strict bool
	// Resolution is the resolution the release claimed; 0 if unknown.
	Resolution release.Resolution
	// Runtime is the expected length of the episodes; 0 if unknown.
	Runtime time.Duration
}

// Check compares a file with what its release promised.
func Check(p Probe, e Expect) api.FileCheck {
	r := api.FileCheck{Audio: languages(p.Audio), Subtitles: languages(p.Subtitles)}
	if p.Duration > 0 {
		r.Duration = p.Duration.Round(time.Second).String()
	}

	if len(p.Video) == 0 {
		r.Problems = append(r.Problems, "the file has no video track")
	} else {
		v := p.Video[0]
		class := api.ResolutionClass(v.Width, v.Height)
		r.Resolution = fmt.Sprintf("%dx%d", v.Width, v.Height)
		switch {
		case class < 720:
			r.Problems = append(r.Problems, fmt.Sprintf("the video is %s, below 720p", r.Resolution))
		case class >= 2160 && e.Resolution < 2160:
			r.Problems = append(r.Problems, fmt.Sprintf("the video is %s; 2160p is reserved for favourites", r.Resolution))
		case e.Resolution >= 1080 && class < 1080:
			r.Problems = append(r.Problems, fmt.Sprintf("the video is %s, but the release claimed %s", r.Resolution, e.Resolution))
		}
	}

	checkAudio(&r, p, e)

	switch {
	case e.Runtime == 0 || p.Duration == 0:
		r.Notes = append(r.Notes, "length not checked: TVDB lists no runtime")
	case p.Duration > e.Runtime*3/2+5*time.Minute:
		r.Problems = append(r.Problems, fmt.Sprintf("the file runs %s, but the episodes run about %s; it holds more than claimed",
			r.Duration, e.Runtime.Round(time.Minute)))
	case p.Duration < e.Runtime/2:
		r.Problems = append(r.Problems, fmt.Sprintf("the file runs %s, but the episodes run about %s; it is incomplete or a sample",
			r.Duration, e.Runtime.Round(time.Minute)))
	}
	return r
}

func checkAudio(r *api.FileCheck, p Probe, e Expect) {
	if len(p.Audio) == 0 {
		r.Problems = append(r.Problems, "the file has no audio track")
		return
	}
	var tagged []release.Language
	for _, t := range p.Audio {
		if l, ok := release.LanguageNamed(t.Language); ok {
			tagged = append(tagged, l)
			if slices.Contains(e.Languages, l) {
				r.Notes = append(r.Notes, fmt.Sprintf("%s audio track found", l.Name()))
				return
			}
		}
	}
	wanted := make([]string, len(e.Languages))
	for i, l := range e.Languages {
		wanted[i] = l.Name()
	}
	switch {
	case len(tagged) == 0:
		r.Notes = append(r.Notes, fmt.Sprintf("audio tracks carry no language tag; %s could not be confirmed", strings.Join(wanted, " or ")))
	case e.Strict:
		r.Problems = append(r.Problems, fmt.Sprintf("the file's audio is %s, with no %s track", strings.Join(r.Audio, ", "), strings.Join(wanted, " or ")))
	default:
		r.Notes = append(r.Notes, fmt.Sprintf("no %s audio track; kept because the release was chosen by hand", strings.Join(wanted, " or ")))
	}
}

// languages names each track's language, or "unknown" without a usable tag.
func languages(tracks []Track) []string {
	out := []string{}
	for _, t := range tracks {
		name := "unknown"
		if l, ok := release.LanguageNamed(t.Language); ok {
			name = l.Name()
		}
		out = append(out, name)
	}
	return out
}

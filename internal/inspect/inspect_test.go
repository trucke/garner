package inspect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trucke/garner/internal/release"
)

func probe(w, h int, minutes float64, audio ...string) Probe {
	p := Probe{Duration: time.Duration(minutes * float64(time.Minute)), Video: []Video{{Codec: "hevc", Width: w, Height: h}}}
	for _, l := range audio {
		p.Audio = append(p.Audio, Track{Codec: "ac3", Language: l})
	}
	return p
}

func TestCheck(t *testing.T) {
	german1080 := Expect{Languages: []release.Language{"de"}, Strict: true, Resolution: 1080, Runtime: 22 * time.Minute}
	tests := []struct {
		name    string
		probe   Probe
		expect  Expect
		problem string // empty: the file passes
		note    string
	}{
		{"matches", probe(1920, 1080, 22.8, "ger", "eng"), german1080, "", "German audio track found"},
		{"wide film is 1080p", probe(1920, 800, 22, "deu"), german1080, "", "German audio track found"},
		{"untagged audio is only noted", probe(1920, 1080, 22, "und"), german1080, "", "could not be confirmed"},
		{"wrong language", probe(1920, 1080, 22, "jpn"), german1080, "with no German track", ""},
		{"wrong language chosen by hand", probe(1920, 1080, 22, "jpn"), Expect{Languages: []release.Language{"de"}, Runtime: 22 * time.Minute}, "", "chosen by hand"},
		{"720p claimed as 1080p", probe(1280, 720, 22, "ger"), german1080, "the release claimed 1080p", ""},
		{"below 720p", probe(720, 576, 22, "ger"), Expect{Languages: []release.Language{"de"}, Strict: true, Runtime: 22 * time.Minute}, "below 720p", ""},
		{"four episodes in one file", probe(1920, 1080, 104, "ger"), german1080, "holds more than claimed", ""},
		{"sample", probe(1920, 1080, 1, "ger"), german1080, "incomplete or a sample", ""},
		{"no runtime known", probe(1920, 1080, 40, "ger"), Expect{Languages: []release.Language{"de"}, Strict: true}, "", "length not checked"},
		{"no video", Probe{Duration: 22 * time.Minute, Audio: []Track{{Language: "ger"}}}, german1080, "no video track", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(tc.probe, tc.expect)
			has := func(list []string, s string) bool {
				return slices.ContainsFunc(list, func(x string) bool { return strings.Contains(x, s) })
			}
			if tc.problem == "" && len(r.Problems) > 0 || tc.problem != "" && !has(r.Problems, tc.problem) || tc.note != "" && !has(r.Notes, tc.note) {
				t.Errorf("Check = problems %q, notes %q; want problem %q, note %q", r.Problems, r.Notes, tc.problem, tc.note)
			}
		})
	}
}

func TestParse(t *testing.T) {
	out := `{"streams":[{"codec_type":"video","codec_name":"hevc","width":1920,"height":1080},
		{"codec_type":"audio","codec_name":"ac3","tags":{"language":"ger","title":"Surround"}},
		{"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}},
		{"codec_type":"video","codec_name":"mjpeg","width":600,"height":900}],
		"format":{"duration":"1367.872000"}}`
	p, err := Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Video) != 1 || len(p.Audio) != 1 || p.Audio[0].Language != "ger" || len(p.Subtitles) != 1 || p.Duration.Round(time.Second) != 1368*time.Second {
		t.Errorf("Parse = %+v", p)
	}
}

// TestProbe runs the real FFprobe on a generated file whose name holds a
// colon, as show titles do. It is skipped without FFmpeg.
func TestProbe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	pr, err := NewProber("ffprobe")
	if err != nil {
		t.Skip(err)
	}
	file := filepath.Join(t.TempDir(), "Show: Title - S01E01.mkv")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=1280x720:duration=2", "-f", "lavfi",
		"-i", "sine=duration=2", "-metadata:s:a:0", "language=ger", "-shortest", file)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	p, err := pr.Probe(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Video) != 1 || p.Video[0].Height != 720 || len(p.Audio) != 1 || p.Audio[0].Language != "ger" {
		t.Errorf("Probe = %+v", p)
	}
	broken := filepath.Join(t.TempDir(), "broken.mkv")
	os.WriteFile(broken, []byte("not a video"), 0o600)
	if _, err := pr.Probe(context.Background(), broken); !errors.Is(err, ErrUnreadable) {
		t.Errorf("broken file: %v", err)
	}
}

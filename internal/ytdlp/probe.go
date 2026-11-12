package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Formats is what one video offers, as yt-dlp reports it.
type Formats struct {
	Title string
	// Video lists the resolutions the video has, highest first, by the
	// shorter side as yt-dlp's "res" sort field and YouTube count them: a
	// portrait 1080x1920 video is 1080p. Labels look like "1080p60".
	Video []Option
	// Audio lists the original audio-only formats, best first. Values are
	// "fmt-<format id>".
	Audio []Option
}

// ErrPlaylist means the URL is a playlist or channel, not one video.
var ErrPlaylist = errors.New("this URL is a playlist or channel; enter the URL of one video")

// maxProbeOutput caps what Probe reads from yt-dlp. One video's JSON is a
// few hundred kilobytes.
const maxProbeOutput = 32 << 20

// ProbeArgs returns the yt-dlp arguments that describe url without
// downloading it.
func ProbeArgs(url string) []string {
	return []string{"--ignore-config", "--no-playlist", "--no-warnings", "--dump-single-json", "--", url}
}

// Probe asks yt-dlp which formats url offers.
func (r Runner) Probe(ctx context.Context, url string) (Formats, error) {
	grace := r.StopGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	cmd := exec.CommandContext(ctx, r.Bin, ProbeArgs(url)...)
	out := &capped{max: maxProbeOutput}
	var errOut capped
	errOut.max = 64 << 10
	cmd.Stdout, cmd.Stderr = out, &errOut
	setProcessGroup(cmd)
	// With buffers as output, Wait also waits for the copying; WaitDelay
	// bounds that when a child keeps the output open.
	cmd.WaitDelay = grace
	err := cmd.Run()
	killProcessGroup(cmd)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Formats{}, ctxErr
	}
	// ErrWaitDelay after a clean exit means a leftover child held the output
	// open; what yt-dlp printed is complete.
	if err != nil && !(errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success()) {
		msg := lastErrorLine(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return Formats{}, &Error{Message: msg}
	}
	if out.over {
		return Formats{}, &Error{Message: "yt-dlp printed more than ukai reads for one video"}
	}
	return parseFormats(out.Bytes())
}

// capped is a buffer that keeps at most max bytes and notes the overflow.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

func lastErrorLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "ERROR:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	if n := len(lines); n > 0 {
		return strings.TrimSpace(lines[n-1])
	}
	return ""
}

type probeJSON struct {
	Type    string `json:"_type"`
	Title   string `json:"title"`
	Formats []struct {
		ID     string   `json:"format_id"`
		Ext    string   `json:"ext"`
		Note   string   `json:"format_note"`
		VCodec string   `json:"vcodec"`
		ACodec string   `json:"acodec"`
		Width  *float64 `json:"width"`
		Height *float64 `json:"height"`
		FPS    *float64 `json:"fps"`
		ABR    *float64 `json:"abr"`
		TBR    *float64 `json:"tbr"`
	} `json:"formats"`
}

// maxNote caps the format note shown in an audio label.
const maxNote = 40

func parseFormats(data []byte) (Formats, error) {
	var v probeJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return Formats{}, fmt.Errorf("read yt-dlp output: %w", err)
	}
	if v.Type == "playlist" || v.Type == "multi_video" {
		return Formats{}, ErrPlaylist
	}
	f := Formats{Title: v.Title}

	fps := map[int]float64{}
	type audio struct {
		opt     Option
		kbps    float64
		base    string
		details []string
	}
	var auds []audio
	for _, x := range v.Formats {
		// yt-dlp writes "none" for a stream a format does not have and leaves
		// the codec out when it does not know it. Storyboards have neither.
		switch {
		case x.VCodec != "none" && x.Height != nil && *x.Height >= 1:
			side := *x.Height
			if x.Width != nil && *x.Width >= 1 && *x.Width < side {
				side = *x.Width
			}
			if side > 10000 {
				continue
			}
			h := int(side)
			rate := 0.0
			if x.FPS != nil {
				rate = *x.FPS
			}
			if cur, ok := fps[h]; !ok || rate > cur {
				fps[h] = rate
			}
		case x.VCodec == "none" && x.ACodec != "none" && formatID.MatchString(x.ID):
			kbps := 0.0
			if x.ABR != nil && *x.ABR > 0 {
				kbps = *x.ABR
			} else if x.TBR != nil && *x.TBR > 0 {
				kbps = *x.TBR
			}
			label := codecName(x.ACodec)
			if kbps > 0 {
				label += " " + strconv.Itoa(int(math.Round(kbps))) + " kbps"
			}
			var details []string
			if x.Ext != "" {
				details = append(details, x.Ext)
			}
			if note := strings.TrimSpace(x.Note); note != "" {
				if r := []rune(note); len(r) > maxNote {
					note = string(r[:maxNote]) + "…"
				}
				details = append(details, note)
			}
			auds = append(auds, audio{Option{"fmt-" + x.ID, withDetails(label, details)}, kbps, label, details})
		}
	}

	heights := make([]int, 0, len(fps))
	for h := range fps {
		heights = append(heights, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(heights)))
	for _, h := range heights {
		label := strconv.Itoa(h) + "p"
		if fps[h] > 30 {
			label += strconv.Itoa(int(math.Round(fps[h])))
		}
		f.Video = append(f.Video, Option{strconv.Itoa(h), label})
	}
	sort.SliceStable(auds, func(i, j int) bool { return auds[i].kbps > auds[j].kbps })
	// Formats that differ only in details the label leaves out keep their
	// own entries; the format ID tells them apart.
	count := map[string]int{}
	for _, a := range auds {
		count[a.opt.Label]++
	}
	for _, a := range auds {
		o := a.opt
		if count[o.Label] > 1 {
			o.Label = withDetails(a.base, append(slices.Clone(a.details), "ID "+strings.TrimPrefix(o.Value, "fmt-")))
		}
		f.Audio = append(f.Audio, o)
	}
	return f, nil
}

// withDetails appends "(webm, medium)" style details to label.
func withDetails(label string, details []string) string {
	if len(details) == 0 {
		return label
	}
	return label + " (" + strings.Join(details, ", ") + ")"
}

func codecName(acodec string) string {
	c := strings.ToLower(acodec)
	switch {
	case c == "":
		return "Audio"
	case strings.HasPrefix(c, "opus"):
		return "Opus"
	case strings.HasPrefix(c, "mp4a"), strings.HasPrefix(c, "aac"):
		return "AAC"
	case strings.HasPrefix(c, "vorbis"):
		return "Vorbis"
	case strings.HasPrefix(c, "mp3"):
		return "MP3"
	case strings.HasPrefix(c, "flac"):
		return "FLAC"
	case strings.HasPrefix(c, "ac-3"), strings.HasPrefix(c, "ec-3"):
		return strings.ToUpper(c)
	}
	if i := strings.IndexByte(c, '.'); i > 0 {
		c = c[:i]
	}
	return strings.ToUpper(c)
}

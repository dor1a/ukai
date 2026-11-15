// Package ytdlp runs the yt-dlp program and reads the lines ukai asks it to
// print.
//
// ukai never imports yt-dlp. It passes --print and --progress-template so that
// yt-dlp writes one tagged line per event to stdout:
//
//	ukai-meta title "Some title"     (JSON value, or NA when the field is missing)
//	ukai-progress 1024 4096 NA 512.5 6   (downloaded total estimate speed eta)
//	ukai-file "/downloads/Some title [id] 320k.mp3"
//
// Every other line is ignored. Error messages come from stderr.
//
// Probe runs yt-dlp with --dump-single-json instead and reads the formats a
// video offers, for the quality lists.
package ytdlp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Option is one choice in a list.
type Option struct {
	Value string
	Label string
}

// MP3 bitrates offered for audio, in kbps.
var MP3Bitrates = []int{320, 256, 192, 128}

var formatID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,32}$`)

// ValidQuality reports whether q can be passed to yt-dlp for audio or video:
//
//	video: "best", or a height in pixels such as "1080" (a cap)
//	audio: "best", "mp3-<kbps>" with a bitrate from MP3Bitrates, or
//	       "fmt-<format id>" for one original audio format
func ValidQuality(audio bool, q string) bool {
	if q == "best" {
		return true
	}
	if !audio {
		h, err := strconv.Atoi(q)
		return err == nil && h > 0 && h <= 10000 && strconv.Itoa(h) == q
	}
	if kbps, ok := strings.CutPrefix(q, "mp3-"); ok {
		n, err := strconv.Atoi(kbps)
		return err == nil && slices.Contains(MP3Bitrates, n) && strconv.Itoa(n) == kbps
	}
	if id, ok := strings.CutPrefix(q, "fmt-"); ok {
		return formatID.MatchString(id)
	}
	return false
}

// QualityLabel describes q for people.
func QualityLabel(audio bool, q string) string {
	switch {
	case q == "" || q == "best":
		if audio {
			return "Best (original)"
		}
		return "Best"
	case !audio:
		return "Up to " + q + "p"
	case strings.HasPrefix(q, "mp3-"):
		return "MP3 " + strings.TrimPrefix(q, "mp3-") + " kbps"
	case strings.HasPrefix(q, "fmt-"):
		return "Original format " + strings.TrimPrefix(q, "fmt-")
	}
	return q
}

// Request describes one download.
type Request struct {
	URL   string
	Audio bool
	// Quality passes ValidQuality. Empty means best.
	Quality string
	// OutDir receives the finished files.
	OutDir string
	// TempDir holds the partial files of this download.
	TempDir string
	// Subdir, when set, is a directory under OutDir the files go to.
	Subdir string
	// Archive, when set, is a yt-dlp download archive: the video is skipped
	// if it is listed there, and added to it once it is downloaded.
	Archive string
}

// Meta is what yt-dlp reports about the video before downloading it.
type Meta struct {
	ID       string
	Title    string
	Uploader string
	// Thumbnail is the site's thumbnail URL, empty when unknown.
	Thumbnail string
	// Duration is in seconds, 0 when unknown.
	Duration float64
}

// Progress is one progress report. Unknown numbers are -1.
type Progress struct {
	Downloaded int64
	// Total is the exact size if known, otherwise yt-dlp's estimate, otherwise -1.
	Total int64
	// Speed is in bytes per second.
	Speed float64
	// ETA is in seconds.
	ETA float64
}

// Handler receives events while yt-dlp runs. Nil fields are skipped. The
// callbacks run on the goroutine that reads yt-dlp's output.
type Handler struct {
	Meta     func(Meta)
	Progress func(Progress)
	File     func(path string)
}

// Error is a failed run.
type Error struct {
	// Message is yt-dlp's last ERROR line, or a description of the exit.
	Message string
}

func (e *Error) Error() string { return e.Message }

// Runner starts yt-dlp.
type Runner struct {
	// Bin is the yt-dlp executable.
	Bin string
	// StopGrace is how long yt-dlp gets to exit after SIGTERM before it is
	// killed, and how long ukai keeps reading output after yt-dlp exits.
	// Zero means 10 seconds.
	StopGrace time.Duration
}

const progressTemplate = "download:ukai-progress %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s %(progress.speed)s %(progress.eta)s"

// outputTemplate names a file by title, video ID and quality. Without the
// quality, the same video in a second quality gets the first file's name:
// yt-dlp then skips a video as already downloaded and overwrites an audio file.
func outputTemplate(audio bool, q string) string {
	const base = "%(title)s [%(id)s]"
	switch {
	case !audio:
		// " 1920x1080". yt-dlp falls back to "720p" or "unknown" when it
		// lacks the width or both sizes.
		return base + " %(resolution)s.%(ext)s"
	case strings.HasPrefix(q, "mp3-"):
		return base + " " + strings.TrimPrefix(q, "mp3-") + "k.%(ext)s"
	default:
		return base + " %(format_id)s.%(ext)s"
	}
}

// Args returns the yt-dlp arguments for req.
func Args(req Request) []string {
	args := []string{
		"--ignore-config",
		"--no-playlist",
		"--newline",
		"--progress",
		"--no-color",
		"--progress-template", progressTemplate,
		"--print", "before_dl:ukai-meta id %(id)j",
		"--print", "before_dl:ukai-meta title %(title)j",
		"--print", "before_dl:ukai-meta uploader %(uploader)j",
		"--print", "before_dl:ukai-meta thumbnail %(thumbnail)j",
		"--print", "before_dl:ukai-meta duration %(duration)j",
		"--print", "after_move:ukai-file %(filepath)j",
		"--paths", filepath.Join(req.OutDir, req.Subdir),
		"--paths", "temp:" + req.TempDir,
		"--output", outputTemplate(req.Audio, req.Quality),
	}
	if req.Archive != "" {
		args = append(args, "--download-archive", req.Archive)
	}
	args = append(args, qualityArgs(req.Audio, req.Quality)...)
	// "--" ends the options, so a URL can never be read as one.
	return append(args, "--", req.URL)
}

// qualityArgs picks formats. Video takes the best video and audio streams,
// optionally capped in height with a format sort (yt-dlp then prefers the
// largest resolution at or below the cap). Audio takes the best audio stream,
// or one chosen original format, and either keeps it as it is or converts the
// best one to MP3.
func qualityArgs(audio bool, q string) []string {
	if !audio {
		args := []string{"--format", "bv*+ba/b"}
		if q != "" && q != "best" {
			args = append(args, "--format-sort", "res:"+q)
		}
		return args
	}
	if id, ok := strings.CutPrefix(q, "fmt-"); ok {
		return []string{"--format", id, "--extract-audio"}
	}
	args := []string{"--format", "ba/b", "--extract-audio"}
	if kbps, ok := strings.CutPrefix(q, "mp3-"); ok {
		args = append(args, "--audio-format", "mp3", "--audio-quality", kbps+"K")
	}
	return args
}

// Run downloads req and reports what yt-dlp prints to h. It returns ctx.Err()
// when ctx is canceled, and an *Error when yt-dlp fails.
//
// Run waits for the yt-dlp process itself, not for its output to close: a
// child that keeps the output open must not keep Run from returning.
func (r Runner) Run(ctx context.Context, req Request, h Handler) error {
	grace := r.StopGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return err
	}
	defer errR.Close()

	cmd := exec.CommandContext(ctx, r.Bin, Args(req)...)
	cmd.Stdout, cmd.Stderr = outW, errW
	setProcessGroup(cmd)
	cmd.WaitDelay = grace
	startErr := cmd.Start()
	// yt-dlp holds its own copies now. Closing ours lets the readers see the
	// end of the output once yt-dlp and its children are gone.
	outW.Close()
	errW.Close()
	if startErr != nil {
		return fmt.Errorf("start yt-dlp: %w", startErr)
	}

	var wg sync.WaitGroup
	var lastError, lastLine string
	p := &parser{h: h}
	wg.Add(2)
	go func() {
		defer wg.Done()
		readLines(outR, p.line)
	}()
	go func() {
		defer wg.Done()
		readLines(errR, func(line string) {
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			lastLine = line
			if strings.HasPrefix(line, "ERROR:") {
				lastError = strings.TrimSpace(strings.TrimPrefix(line, "ERROR:"))
			}
		})
	}()

	// On cancel, cmd.Cancel sends SIGTERM to the group. Whatever in the group
	// is still there after the grace period gets SIGKILL.
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-exited:
			case <-time.After(grace):
				killProcessGroup(cmd)
			}
		case <-exited:
		}
	}()
	waitErr := cmd.Wait()
	close(exited)
	// yt-dlp starts ffmpeg in its group. Kill whatever of the group outlived
	// yt-dlp. A group ID stays taken while any member lives, so this cannot
	// reach another job's group; with no member left there is nothing to kill.
	killProcessGroup(cmd)

	// Read what is still in the pipes. A process that left the group can hold
	// them open, so stop reading after the grace period.
	readers := make(chan struct{})
	go func() {
		wg.Wait()
		close(readers)
	}()
	select {
	case <-readers:
	case <-time.After(grace):
		outR.Close()
		errR.Close()
		<-readers
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if waitErr == nil {
		return nil
	}
	msg := lastError
	if msg == "" {
		msg = lastLine
	}
	var exitErr *exec.ExitError
	if msg == "" && errors.As(waitErr, &exitErr) {
		msg = fmt.Sprintf("yt-dlp exited with code %d", exitErr.ExitCode())
	}
	if msg == "" {
		msg = waitErr.Error()
	}
	return &Error{Message: msg}
}

func readLines(rd io.Reader, fn func(string)) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fn(sc.Text())
	}
	// A line longer than the buffer stops the scanner. Drain the rest so
	// yt-dlp never blocks on a full pipe.
	io.Copy(io.Discard, rd)
}

// parser turns the tagged lines of one run into Handler calls.
type parser struct {
	h    Handler
	meta Meta
}

// line handles one stdout line. The ukai-meta lines of a video come in the
// order of the --print flags in Args: id first, duration last.
func (p *parser) line(line string) {
	tag, rest, _ := strings.Cut(line, " ")
	switch tag {
	case "ukai-meta":
		key, value, _ := strings.Cut(rest, " ")
		switch key {
		case "id":
			p.meta = Meta{ID: jsonString(value)}
		case "title":
			p.meta.Title = jsonString(value)
		case "uploader":
			p.meta.Uploader = jsonString(value)
		case "thumbnail":
			p.meta.Thumbnail = jsonString(value)
		case "duration":
			p.meta.Duration = max(number(value), 0)
			if p.h.Meta != nil {
				p.h.Meta(p.meta)
			}
		}
	case "ukai-progress":
		if pr, ok := parseProgress(rest); ok && p.h.Progress != nil {
			p.h.Progress(pr)
		}
	case "ukai-file":
		if s := jsonString(rest); s != "" && p.h.File != nil {
			p.h.File(s)
		}
	}
}

// parseProgress reads "downloaded total estimate speed eta". yt-dlp prints NA
// for numbers it does not know.
func parseProgress(s string) (Progress, bool) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return Progress{}, false
	}
	downloaded := number(f[0])
	if downloaded < 0 {
		return Progress{}, false
	}
	total := number(f[1])
	if total <= 0 {
		total = number(f[2])
	}
	if total <= 0 {
		total = -1
	}
	return Progress{
		Downloaded: int64(downloaded),
		Total:      int64(total),
		Speed:      number(f[3]),
		ETA:        number(f[4]),
	}, true
}

// number parses a number printed by yt-dlp, or returns -1 for NA and
// anything else it cannot read.
func number(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v != v || v < 0 { // v != v catches NaN
		return -1
	}
	return v
}

// jsonString decodes a value printed with yt-dlp's j conversion. NA and
// anything that is not a JSON string become "".
func jsonString(s string) string {
	var v string
	if json.Unmarshal([]byte(strings.TrimSpace(s)), &v) != nil {
		return ""
	}
	return v
}

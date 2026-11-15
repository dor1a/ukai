package ytdlp

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Entry is one video in a channel or playlist listing.
type Entry struct {
	// Key is yt-dlp's extractor key in lower case, as its download archive
	// writes it ("youtube").
	Key   string
	ID    string
	URL   string
	Title string
}

// Listing is what a channel or playlist holds, newest first as the site
// lists it.
type Listing struct {
	// Title is the playlist or tab title, Channel the channel's name.
	Title   string
	Channel string
	Entries []Entry
}

// Errors returned by List.
var (
	ErrNotPlaylist = errors.New("this URL is one video; subscribe to a channel or playlist")
	ErrNested      = errors.New("this URL lists playlists, not videos; use the channel's Videos tab or one playlist")
)

// maxListOutput caps what List reads. A listing entry is about a kilobyte, so
// this allows channels with tens of thousands of videos.
const maxListOutput = 64 << 20

// ListArgs returns the yt-dlp arguments that list url without downloading.
// A limit above zero keeps only the first limit entries.
func ListArgs(url string, limit int) []string {
	args := []string{"--ignore-config", "--flat-playlist", "--no-warnings", "--dump-single-json"}
	if limit > 0 {
		args = append(args, "--playlist-end", strconv.Itoa(limit))
	}
	return append(args, "--", url)
}

// List asks yt-dlp what the channel or playlist at url holds.
func (r Runner) List(ctx context.Context, url string, limit int) (Listing, error) {
	grace := r.StopGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	cmd := exec.CommandContext(ctx, r.Bin, ListArgs(url, limit)...)
	out := &capped{max: maxListOutput}
	var errOut capped
	errOut.max = 64 << 10
	cmd.Stdout, cmd.Stderr = out, &errOut
	setProcessGroup(cmd)
	cmd.WaitDelay = grace
	err := cmd.Run()
	killProcessGroup(cmd)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Listing{}, ctxErr
	}
	if err != nil && !(errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success()) {
		msg := lastErrorLine(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return Listing{}, &Error{Message: msg}
	}
	if out.over {
		return Listing{}, &Error{Message: "yt-dlp printed more than ukai reads for one listing"}
	}
	return parseListing(out.Bytes())
}

func parseListing(data []byte) (Listing, error) {
	var raw struct {
		Type     string `json:"_type"`
		Title    string `json:"title"`
		Channel  string `json:"channel"`
		Uploader string `json:"uploader"`
		Entries  []struct {
			Type  string `json:"_type"`
			Key   string `json:"ie_key"`
			ID    string `json:"id"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Listing{}, &Error{Message: "yt-dlp printed a listing ukai cannot read: " + err.Error()}
	}
	if raw.Type != "playlist" {
		return Listing{}, ErrNotPlaylist
	}
	l := Listing{Title: raw.Title, Channel: raw.Channel}
	if l.Channel == "" {
		l.Channel = raw.Uploader
	}
	nested := 0
	for _, e := range raw.Entries {
		if e.Type == "playlist" {
			nested++
			continue
		}
		if e.ID == "" || !strings.HasPrefix(e.URL, "https://") && !strings.HasPrefix(e.URL, "http://") {
			continue
		}
		key := strings.ToLower(e.Key)
		if key == "" {
			key = "youtube"
		}
		l.Entries = append(l.Entries, Entry{Key: key, ID: e.ID, URL: e.URL, Title: e.Title})
	}
	if len(l.Entries) == 0 && nested > 0 {
		return Listing{}, ErrNested
	}
	return l, nil
}

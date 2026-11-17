package source

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// youtubeHosts is the allow-list of hosts; anything else never reaches yt-dlp.
var youtubeHosts = map[string]bool{
	"youtube.com":       true,
	"www.youtube.com":   true,
	"m.youtube.com":     true,
	"music.youtube.com": true,
	"youtu.be":          true,
}

var (
	youtubeID   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	youtubeTime = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$`)
)

// matchYouTube recognizes youtube.com/watch, youtu.be, /shorts/, /live/ and
// /embed/ links, with or without the scheme.
func matchYouTube(raw string) (Link, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\n") {
		return Link{}, ErrNoMatch
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Link{}, ErrNoMatch
	}
	host := strings.ToLower(u.Hostname())
	if !youtubeHosts[host] {
		return Link{}, ErrNoMatch
	}

	q := u.Query()
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	var id string
	switch {
	case host == "youtu.be":
		id = segments[0]
	case segments[0] == "watch":
		id = q.Get("v")
	case len(segments) >= 2 && (segments[0] == "shorts" || segments[0] == "live" ||
		segments[0] == "embed" || segments[0] == "v"):
		id = segments[1]
	}
	if !youtubeID.MatchString(id) {
		return Link{}, ErrNotVideo
	}

	start := q.Get("t")
	if start == "" {
		start = q.Get("start")
	}
	return Link{
		Source:  "youtube",
		ID:      id,
		URL:     "https://www.youtube.com/watch?v=" + id,
		StartAt: parseYouTubeTime(start),
	}, nil
}

// parseYouTubeTime reads the t= parameter: "90", "90s", "1m30s", "1h2m3s".
func parseYouTubeTime(s string) time.Duration {
	if s == "" {
		return 0
	}
	m := youtubeTime.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0
	}
	var total int64
	for i, mult := range []int64{3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 32)
		if err != nil {
			return 0
		}
		total += n * mult
	}
	return time.Duration(total) * time.Second
}

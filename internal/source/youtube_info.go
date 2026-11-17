package source

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ytInfo is the part of `yt-dlp -J` output the bot uses.
type ytInfo struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Channel    string     `json:"channel"`
	Uploader   string     `json:"uploader"`
	Duration   float64    `json:"duration"`
	Thumbnail  string     `json:"thumbnail"`
	IsLive     bool       `json:"is_live"`
	LiveStatus string     `json:"live_status"`
	Formats    []ytFormat `json:"formats"`

	// Set when a format selector was passed: the formats yt-dlp chose.
	RequestedFormats []ytFormat `json:"requested_formats"`
	ytFormat
}

type ytFormat struct {
	FormatID       string            `json:"format_id"`
	URL            string            `json:"url"`
	Ext            string            `json:"ext"`
	Protocol       string            `json:"protocol"`
	VCodec         string            `json:"vcodec"`
	ACodec         string            `json:"acodec"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	TBR            float64           `json:"tbr"`
	Filesize       int64             `json:"filesize"`
	FilesizeApprox int64             `json:"filesize_approx"`
	FormatNote     string            `json:"format_note"`
	HTTPHeaders    map[string]string `json:"http_headers"`
}

func (f ytFormat) hasVideo() bool {
	return f.VCodec != "" && f.VCodec != "none" && f.Width > 0 && f.Height > 0
}

func (f ytFormat) hasAudio() bool { return f.ACodec != "" && f.ACodec != "none" }

func (f ytFormat) isH264() bool { return strings.HasPrefix(f.VCodec, "avc1") }

// res is the smaller side of the picture, the way yt-dlp's "res" sort field
// counts it, so that vertical Shorts get a sensible quality.
func (f ytFormat) res() int {
	return min(f.Width, f.Height)
}

// bytes estimates the size of the whole stream.
func (f ytFormat) bytes(duration time.Duration) int64 {
	switch {
	case f.Filesize > 0:
		return f.Filesize
	case f.FilesizeApprox > 0:
		return f.FilesizeApprox
	default:
		return int64(f.TBR * 1000 / 8 * duration.Seconds())
	}
}

var noteRes = regexp.MustCompile(`^(\d{3,4})p`)

func parseYouTubeInfo(data []byte) (*Info, error) {
	var raw ytInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode yt-dlp output: %w", err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("decode yt-dlp output: no video id")
	}

	info := &Info{
		ID:        raw.ID,
		Title:     raw.Title,
		Channel:   raw.Channel,
		Duration:  time.Duration(raw.Duration * float64(time.Second)),
		Thumbnail: raw.Thumbnail,
		Live:      raw.IsLive || raw.LiveStatus == "is_live" || raw.LiveStatus == "is_upcoming" || raw.LiveStatus == "post_live",
	}
	if info.Channel == "" {
		info.Channel = raw.Uploader
	}
	if info.Title == "" {
		info.Title = raw.ID
	}

	var video []ytFormat
	for _, f := range raw.Formats {
		switch {
		case f.hasVideo():
			video = append(video, f)
		case f.hasAudio() && f.Ext == "m4a":
			info.AudioBytes = max(info.AudioBytes, f.bytes(info.Duration))
		}
	}
	if info.AudioBytes == 0 {
		info.AudioBytes = int64(130_000 / 8 * info.Duration.Seconds())
	}

	seen := 0
	for _, tier := range Tiers {
		// The best resolution within the tier, as `-S res:<tier>` would pick it.
		best := 0
		for _, f := range video {
			if r := f.res(); r <= tier && r > best {
				best = r
			}
		}
		if best == 0 || best == seen {
			continue
		}
		seen = best
		info.Qualities = append(info.Qualities, buildQuality(tier, best, video, info))
	}
	return info, nil
}

func buildQuality(tier, res int, video []ytFormat, info *Info) Quality {
	duration := info.Duration
	q := Quality{Tier: tier, Res: res, Label: strconv.Itoa(res) + "p", Recode: true}
	for _, f := range video {
		if f.res() == res && f.isH264() {
			q.Recode = false
		}
	}

	var sized, unsized int64
	for _, f := range video {
		// Only H.264 formats count when there are any: those are the ones downloaded.
		if f.res() != res || f.isH264() == q.Recode {
			continue
		}
		if m := noteRes.FindStringSubmatch(f.FormatNote); m != nil {
			q.Label = m[1] + "p"
		}
		// A format that carries its own sound (the old progressive 360p) is
		// downloaded alone, so its audio must not be counted twice.
		size := f.bytes(duration)
		if f.hasAudio() {
			size = max(size-info.AudioBytes, 0)
		}
		// Formats without a known size (HLS) only report peak bitrate, which
		// overestimates a lot, so they are a fallback.
		if f.Filesize > 0 || f.FilesizeApprox > 0 {
			sized = max(sized, size)
		} else {
			unsized = max(unsized, size)
		}
	}
	q.VideoBytes = sized
	if sized == 0 {
		q.VideoBytes = unsized
	}
	return q
}

// Package source hides the downloader behind the Source interface, so that a
// new platform is one more implementation and nothing else changes.
package source

import (
	"context"
	"errors"
	"time"
)

// Format is the kind of file the user receives.
type Format string

const (
	MP4 Format = "mp4"
	MP3 Format = "mp3"
)

// MP3Bitrate is the fixed MP3 bitrate in bits per second.
const MP3Bitrate = 192_000

// Tiers are the video qualities offered to the user: the smaller side of the
// picture is at most this many pixels.
var Tiers = []int{360, 720, 1080}

var (
	// ErrNoMatch means the text is not a link this source understands.
	ErrNoMatch = errors.New("source: link is not supported")
	// ErrNotVideo means the link belongs to the source but does not point
	// at a single video (a playlist, a channel and so on).
	ErrNotVideo = errors.New("source: link is not a video")
)

// Link is a recognized video link.
type Link struct {
	Source  string        // source name, e.g. "youtube"
	ID      string        // video ID
	URL     string        // canonical URL rebuilt from the ID
	StartAt time.Duration // start offset from the link (?t=90), 0 if none
}

// Quality is one video quality available for a particular video.
type Quality struct {
	Tier   int    // one of Tiers
	Res    int    // actual smaller side of the picture, e.g. 480
	Label  string // "480p"
	Recode bool   // no H.264 at this resolution, video has to be re-encoded
	// VideoBytes is the estimated size of the whole video stream.
	VideoBytes int64
}

// Info is the metadata obtained without downloading.
type Info struct {
	ID        string
	Title     string
	Channel   string
	Duration  time.Duration
	Thumbnail string
	Live      bool
	Qualities []Quality // ascending, empty if there is no video stream
	// AudioBytes is the estimated size of the whole AAC audio stream.
	AudioBytes int64
	// Auth means the video only opens for a signed-in account (age
	// restriction), so the download has to use the account too.
	Auth bool
}

// Quality returns the best quality that does not exceed tier.
func (i *Info) Quality(tier int) (Quality, bool) {
	var best Quality
	found := false
	for _, q := range i.Qualities {
		if q.Tier <= tier {
			best, found = q, true
		}
	}
	return best, found
}

// EstimateSize predicts the size in bytes of a piece of the given length.
// tier is ignored for MP3.
func (i *Info) EstimateSize(f Format, tier int, length time.Duration) int64 {
	if f == MP3 {
		const tagsAndCover = 150 << 10
		return int64(length.Seconds()*MP3Bitrate/8) + tagsAndCover
	}
	q, ok := i.Quality(tier)
	if !ok || i.Duration <= 0 {
		return 0
	}
	whole := float64(q.VideoBytes + i.AudioBytes)
	const containerOverhead = 1.02
	return int64(whole * length.Seconds() / i.Duration.Seconds() * containerOverhead)
}

// Request describes the file to produce.
type Request struct {
	Link   Link
	Info   *Info
	Format Format
	Tier   int // video quality, ignored for MP3

	// Start and End bound the fragment; both are zero for the whole video.
	Start, End time.Duration
	// Precise asks for frame-accurate boundaries at the cost of re-encoding.
	Precise bool

	// Dir is an existing empty directory for this job only.
	Dir string
}

// Whole reports that the entire video is requested.
func (r Request) Whole() bool { return r.Start == 0 && r.End == 0 }

// Stage is a phase of Download.
type Stage int

const (
	StageDownload Stage = iota
	StageConvert
)

// Progress is reported from Download. Percent is negative when unknown.
type Progress struct {
	Stage   Stage
	Percent float64
}

// Result is the ready file.
type Result struct {
	Path     string
	Thumb    string // small JPEG preview, may be empty
	Size     int64
	Duration time.Duration
	Width    int
	Height   int
}

// Source is one platform the bot can download from.
type Source interface {
	Name() string
	// Match recognizes a link. It returns ErrNoMatch for foreign links and
	// ErrNotVideo for links of this platform that are not a single video.
	Match(rawURL string) (Link, error)
	// Probe returns metadata without downloading anything.
	Probe(ctx context.Context, link Link) (*Info, error)
	// Download produces the file in req.Dir. It must stop all child
	// processes when ctx is cancelled.
	Download(ctx context.Context, req Request, progress func(Progress)) (*Result, error)
}

// Kind classifies a failure so the bot can explain it to the user.
type Kind int

const (
	KindUnknown       Kind = iota // anything else, most likely the downloader is outdated
	KindUnavailable               // deleted or never existed
	KindPrivate                   // private video
	KindAgeRestricted             // needs a signed-in adult account
	KindLive                      // live stream or a premiere that has not started
	KindMembersOnly               // channel members only
	KindGeo                       // not available in the server's country
	KindBlocked                   // the platform throttles or blocks the server
)

// Error is a classified failure of Probe or Download.
type Error struct {
	Kind   Kind
	Detail string // raw downloader output for the log, not for the user
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "source: download failed"
	}
	return "source: " + e.Detail
}

// KindOf extracts the failure kind, KindUnknown if err is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

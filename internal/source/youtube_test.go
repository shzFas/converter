package source

import (
	"errors"
	"testing"
	"time"
)

func TestMatchYouTube(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	tests := []struct {
		in    string
		start time.Duration
	}{
		{"https://www.youtube.com/watch?v=" + id, 0},
		{"https://youtube.com/watch?v=" + id + "&list=PL123&index=2", 0},
		{"http://m.youtube.com/watch?feature=share&v=" + id, 0},
		{"https://music.youtube.com/watch?v=" + id, 0},
		{"https://youtu.be/" + id, 0},
		{"youtu.be/" + id, 0},
		{"www.youtube.com/watch?v=" + id, 0},
		{"https://www.youtube.com/shorts/" + id, 0},
		{"https://www.youtube.com/shorts/" + id + "?feature=share", 0},
		{"https://www.youtube.com/live/" + id, 0},
		{"https://www.youtube.com/embed/" + id, 0},
		{"https://YouTube.com/watch?v=" + id, 0},
		{"https://youtu.be/" + id + "?t=90", 90 * time.Second},
		{"https://youtu.be/" + id + "?t=90s", 90 * time.Second},
		{"https://www.youtube.com/watch?v=" + id + "&t=1m30s", 90 * time.Second},
		{"https://www.youtube.com/watch?v=" + id + "&t=1h2m3s", 3723 * time.Second},
		{"https://www.youtube.com/embed/" + id + "?start=45", 45 * time.Second},
		{"https://youtu.be/" + id + "?t=abc", 0},
	}
	for _, tt := range tests {
		link, err := matchYouTube(tt.in)
		if err != nil {
			t.Errorf("matchYouTube(%q): %v", tt.in, err)
			continue
		}
		if link.ID != id || link.URL != "https://www.youtube.com/watch?v="+id || link.Source != "youtube" {
			t.Errorf("matchYouTube(%q) = %+v", tt.in, link)
		}
		if link.StartAt != tt.start {
			t.Errorf("matchYouTube(%q) start = %v, want %v", tt.in, link.StartAt, tt.start)
		}
	}
}

func TestMatchYouTubeRejects(t *testing.T) {
	tests := map[string]error{
		"":                         ErrNoMatch,
		"привет":                   ErrNoMatch,
		"1:30-2:45":                ErrNoMatch,
		"https://vimeo.com/123456": ErrNoMatch,
		"https://youtube.com.evil.org/watch?v=dQw4w9WgXcQ":  ErrNoMatch,
		"https://notyoutube.com/watch?v=dQw4w9WgXcQ":        ErrNoMatch,
		"ftp://youtube.com/watch?v=dQw4w9WgXcQ":             ErrNoMatch,
		"file:///etc/passwd":                                ErrNoMatch,
		"--exec=rm":                                         ErrNoMatch,
		"https://www.youtube.com/playlist?list=PL123":       ErrNotVideo,
		"https://www.youtube.com/@channel":                  ErrNotVideo,
		"https://www.youtube.com/":                          ErrNotVideo,
		"https://www.youtube.com/watch?v=short":             ErrNotVideo,
		"https://youtu.be/dQw4w9WgXcQ;rm":                   ErrNotVideo,
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ%20--x": ErrNotVideo,
	}
	for in, want := range tests {
		if _, err := matchYouTube(in); !errors.Is(err, want) {
			t.Errorf("matchYouTube(%q) error = %v, want %v", in, err, want)
		}
	}
}

// A trimmed `yt-dlp -J` answer: a 10-minute video with H.264 up to 720p,
// VP9 only at 1080p, and HLS duplicates that report no file size.
const sampleInfo = `{
 "id": "abc", "title": "Sample", "channel": "", "uploader": "Uploader", "duration": 600,
 "thumbnail": "https://i.ytimg.com/vi/abc/hq.jpg", "is_live": false, "live_status": "not_live",
 "formats": [
  {"format_id": "sb0", "ext": "mhtml", "vcodec": "none", "acodec": "none", "width": 160, "height": 90},
  {"format_id": "234", "ext": "mp4", "vcodec": "none", "protocol": "m3u8_native"},
  {"format_id": "140", "ext": "m4a", "vcodec": "none", "acodec": "mp4a.40.2", "filesize": 10000000, "tbr": 129},
  {"format_id": "251", "ext": "webm", "vcodec": "none", "acodec": "opus", "filesize": 9000000},
  {"format_id": "230", "ext": "mp4", "vcodec": "avc1.4D401E", "acodec": "none", "width": 640, "height": 360, "tbr": 800, "protocol": "m3u8_native"},
  {"format_id": "134", "ext": "mp4", "vcodec": "avc1.4D401E", "acodec": "none", "width": 640, "height": 360, "filesize": 20000000, "format_note": "360p"},
  {"format_id": "18", "ext": "mp4", "vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "width": 640, "height": 360, "filesize_approx": 29000000, "format_note": "360p"},
  {"format_id": "243", "ext": "webm", "vcodec": "vp9", "acodec": "none", "width": 640, "height": 360, "filesize": 99000000, "format_note": "360p"},
  {"format_id": "135", "ext": "mp4", "vcodec": "avc1.4D401F", "acodec": "none", "width": 854, "height": 480, "filesize": 30000000, "format_note": "480p"},
  {"format_id": "298", "ext": "mp4", "vcodec": "avc1.4D4020", "acodec": "none", "width": 1280, "height": 720, "filesize_approx": 60000000, "format_note": "720p60"},
  {"format_id": "248", "ext": "webm", "vcodec": "vp9", "acodec": "none", "width": 1920, "height": 1080, "filesize": 90000000, "format_note": "1080p"}
 ]
}`

func TestParseYouTubeInfo(t *testing.T) {
	info, err := parseYouTubeInfo([]byte(sampleInfo))
	if err != nil {
		t.Fatal(err)
	}
	if info.Channel != "Uploader" || info.Duration != 10*time.Minute || info.Live {
		t.Errorf("info = %+v", info)
	}
	if info.AudioBytes != 10000000 {
		t.Errorf("AudioBytes = %d, want the m4a size", info.AudioBytes)
	}

	want := []Quality{
		// The VP9 and HLS variants of 360p must not inflate the estimate, and
		// neither must format 18, whose 29 MB already include 10 MB of audio.
		{Tier: 360, Res: 360, Label: "360p", VideoBytes: 20000000},
		{Tier: 720, Res: 720, Label: "720p", VideoBytes: 60000000},
		{Tier: 1080, Res: 1080, Label: "1080p", Recode: true, VideoBytes: 90000000},
	}
	if len(info.Qualities) != len(want) {
		t.Fatalf("qualities = %+v", info.Qualities)
	}
	for i, q := range info.Qualities {
		if q != want[i] {
			t.Errorf("quality %d = %+v, want %+v", i, q, want[i])
		}
	}

	if q, ok := info.Quality(720); !ok || q.Res != 720 {
		t.Errorf("Quality(720) = %+v, %v", q, ok)
	}
	// Half of the video is half of (60 + 10) MB plus container overhead.
	if got := info.EstimateSize(MP4, 720, 5*time.Minute); got != 35700000 {
		t.Errorf("EstimateSize(MP4 720, 5m) = %d", got)
	}
	// 192 kbps is 24 000 bytes a second.
	if got := info.EstimateSize(MP3, 0, time.Minute); got != 60*24000+150<<10 {
		t.Errorf("EstimateSize(MP3, 1m) = %d", got)
	}
}

func TestParseYouTubeInfoShapes(t *testing.T) {
	// A vertical Short is 1080x1920: its quality is the smaller side.
	short := `{"id": "s", "title": "Short", "duration": 30, "formats": [
	 {"format_id": "1", "vcodec": "avc1", "width": 360, "height": 640, "filesize": 1000},
	 {"format_id": "2", "vcodec": "avc1", "width": 1080, "height": 1920, "filesize": 5000}]}`
	info, err := parseYouTubeInfo([]byte(short))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Qualities) != 2 || info.Qualities[0].Res != 360 || info.Qualities[1].Tier != 1080 {
		t.Errorf("short qualities = %+v", info.Qualities)
	}

	// A 480p-only video offers one quality, not the same file three times.
	low := `{"id": "l", "title": "Low", "duration": 30, "formats": [
	 {"format_id": "1", "vcodec": "avc1", "width": 854, "height": 480, "filesize": 1000}]}`
	info, err = parseYouTubeInfo([]byte(low))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Qualities) != 1 || info.Qualities[0].Tier != 720 || info.Qualities[0].Label != "480p" {
		t.Errorf("low qualities = %+v", info.Qualities)
	}

	live := `{"id": "x", "title": "Live", "duration": 0, "live_status": "is_live", "formats": []}`
	if info, err = parseYouTubeInfo([]byte(live)); err != nil || !info.Live {
		t.Errorf("live = %+v, %v", info, err)
	}
	if _, err := parseYouTubeInfo([]byte(`{"title": "no id"}`)); err == nil {
		t.Error("info without an id was accepted")
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]Kind{
		"ERROR: [youtube] abc: Private video. Sign in if you've been granted access to this video": KindPrivate,
		"ERROR: [youtube] abc: Video unavailable":                                                  KindUnavailable,
		"ERROR: [youtube] abc: This video has been removed by the uploader":                        KindUnavailable,
		"ERROR: [youtube] abc: Sign in to confirm your age. This video may be inappropriate":       KindAgeRestricted,
		"ERROR: [youtube] abc: Sign in to confirm you’re not a bot. Use --cookies-from-browser":    KindBlocked,
		"ERROR: unable to download video data: HTTP Error 403: Forbidden":                          KindBlocked,
		"ERROR: [youtube] abc: HTTP Error 429: Too Many Requests":                                  KindBlocked,
		"ERROR: [youtube] abc: The uploader has not made this video available in your country":     KindGeo,
		"ERROR: [youtube] abc: Join this channel to get access to members-only content":            KindMembersOnly,
		"ERROR: [youtube] abc: This live event will begin in 3 hours.":                             KindLive,
		"ERROR: [youtube] abc: Requested format is not available. Use --list-formats":              KindUnknown,
		"ERROR: [youtube] abc: Unable to extract nsig function":                                    KindUnknown,
		"": KindUnknown,
	}
	for stderr, want := range tests {
		if got := classify(stderr); got != want {
			t.Errorf("classify(%q) = %v, want %v", stderr, got, want)
		}
	}
}

func TestDownloadTracker(t *testing.T) {
	var got []Progress
	tr := &downloadTracker{expected: 1000, report: func(p Progress) { got = append(got, p) }}
	for _, line := range []string{
		"[youtube] abc: Downloading webpage",
		"PROGRESS downloading 400 800 NA",   // video: 400 of 1000 expected
		"PROGRESS finished 800 800 NA",      // video done
		"PROGRESS downloading 100 NA 200.0", // audio, size only estimated
		"PROGRESS finished 200 200 NA",
		"[Merger] Merging formats into \"media.mp4\"",
	} {
		tr.line(line)
	}
	want := []Progress{
		{StageDownload, 40}, {StageDownload, 80}, {StageDownload, 90}, {StageDownload, 99}, {StageConvert, -1},
	}
	if len(got) != len(want) {
		t.Fatalf("progress = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("progress[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFFmpegProgress(t *testing.T) {
	var got []float64
	line := ffmpegProgress(10*time.Second, StageDownload, func(p Progress) { got = append(got, p.Percent) })
	for _, l := range []string{"frame=10", "out_time_us=N/A", "out_time_us=2500000", "progress=continue", "out_time_us=10400000"} {
		line(l)
	}
	if len(got) != 3 || got[0] != 0 || got[1] != 25 || got[2] != 99 {
		t.Errorf("percents = %v, want [0 25 99]", got)
	}
}

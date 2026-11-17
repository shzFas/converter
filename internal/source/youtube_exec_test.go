//go:build unix

package source

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeTools puts shell scripts in place of yt-dlp, ffmpeg and ffprobe.
// The fake ffmpeg counts its calls in a file and hangs on the calls listed
// in hangOn, the way a stalled CDN connection makes the real one hang.
func fakeTools(t *testing.T, hangOn string, duration string) (*YouTube, string) {
	t.Helper()
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	counter := filepath.Join(dir, "ffmpeg-calls")

	yt := NewYouTube(YouTubeConfig{
		YtDlp: script("yt-dlp", `cat <<'JSON'
{"id": "x", "title": "t", "duration": 100, "requested_formats": [
 {"format_id": "v", "url": "https://example.com/v", "vcodec": "avc1.4D401E", "protocol": "m3u8_native", "width": 640, "height": 360,
  "http_headers": {"User-Agent": "test"}},
 {"format_id": "a", "url": "https://example.com/a", "vcodec": "none", "protocol": "m3u8_native"}]}
JSON
`),
		FFmpeg: script("ffmpeg", `n=$(($(cat "`+counter+`" 2>/dev/null || echo 0) + 1))
echo $n > "`+counter+`"
case " `+hangOn+` " in *" $n "*) echo out_time_us=1000000; sleep 30 ;; esac
for last; do :; done
echo out_time_us=5000000
echo progress=end
: > "$last"
`),
		FFprobe: script("ffprobe", `echo '{"streams": [{"codec_type": "video", "codec_name": "h264", "width": 640, "height": 360},
 {"codec_type": "audio", "codec_name": "aac"}], "format": {"duration": "`+duration+`"}}'
`),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	yt.stall = time.Second
	return yt, counter
}

func fragmentRequest(t *testing.T) Request {
	return Request{
		Link:   Link{Source: "youtube", ID: "x", URL: "https://www.youtube.com/watch?v=x"},
		Info:   &Info{ID: "x", Duration: 100 * time.Second, Qualities: []Quality{{Tier: 360, Res: 360}}},
		Format: MP4, Tier: 360,
		Start: 10 * time.Second, End: 20 * time.Second, Precise: true,
		Dir: t.TempDir(),
	}
}

func calls(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

func TestStalledFragmentIsRetried(t *testing.T) {
	yt, counter := fakeTools(t, "1 2", "10.0")
	req := fragmentRequest(t)

	started := time.Now()
	res, err := yt.Download(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("took %s: the hung ffmpeg was waited for instead of being killed", took)
	}
	// Two hung attempts, the successful one and the thumbnail.
	if n := calls(t, counter); n != 4 {
		t.Errorf("ffmpeg calls = %d, want 4", n)
	}
	if res.Path != filepath.Join(req.Dir, "media.mp4") || res.Width != 640 || res.Duration != 10*time.Second {
		t.Errorf("result = %+v", res)
	}
}

func TestFragmentGivesUpAfterThreeStalls(t *testing.T) {
	yt, counter := fakeTools(t, "1 2 3", "10.0")
	_, err := yt.Download(context.Background(), fragmentRequest(t), nil)
	if KindOf(err) != KindBlocked {
		t.Fatalf("error = %v, want a blocked-kind error", err)
	}
	if n := calls(t, counter); n != partAttempts {
		t.Errorf("ffmpeg calls = %d, want %d", n, partAttempts)
	}
}

func TestIncompleteFragmentIsRejected(t *testing.T) {
	// ffmpeg exits fine, but the file is 3 seconds long instead of 10.
	yt, counter := fakeTools(t, "", "3.0")
	_, err := yt.Download(context.Background(), fragmentRequest(t), nil)
	if err == nil || !strings.Contains(err.Error(), "instead of") {
		t.Fatalf("error = %v, want the incomplete fragment to be rejected", err)
	}
	if n := calls(t, counter); n != partAttempts {
		t.Errorf("ffmpeg calls = %d, want %d", n, partAttempts)
	}
}

func TestCancelKillsDownload(t *testing.T) {
	yt, _ := fakeTools(t, "1", "10.0")
	yt.stall = time.Minute
	cause := errors.New("user pressed cancel")
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(100*time.Millisecond, func() { cancel(cause) })

	started := time.Now()
	_, err := yt.Download(ctx, fragmentRequest(t), nil)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the cancellation cause", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("took %s: the child process outlived the cancellation", took)
	}
}

// ageGate is a fake yt-dlp that opens the video only when cookies are passed
// and records the arguments of every call.
func ageGate(t *testing.T, cfg YouTubeConfig) (*YouTube, func() []string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	path := filepath.Join(dir, "yt-dlp")
	body := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "$*" in
*--cookies*) echo '{"id": "x", "title": "t", "duration": 100, "formats": []}' ;;
*) echo "ERROR: [youtube] x: Sign in to confirm your age. This video may be inappropriate for some users." >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.YtDlp = path
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewYouTube(cfg), func() []string {
		raw, _ := os.ReadFile(calls)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

func TestAgeRestrictedWithoutAccount(t *testing.T) {
	yt, calls := ageGate(t, YouTubeConfig{})
	_, err := yt.Probe(context.Background(), Link{ID: "x", URL: "https://www.youtube.com/watch?v=x"})
	if KindOf(err) != KindAgeRestricted {
		t.Fatalf("error = %v, want age-restricted", err)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("yt-dlp calls = %q, want one", got)
	}
}

func TestAgeRestrictedUsesAccountOnlyWhenNeeded(t *testing.T) {
	yt, calls := ageGate(t, YouTubeConfig{CookiesBrowser: "chrome"})
	info, err := yt.Probe(context.Background(), Link{ID: "x", URL: "https://www.youtube.com/watch?v=x"})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Auth {
		t.Error("info.Auth is not set: the download would go without the account and fail")
	}
	got := calls()
	if len(got) != 2 || strings.Contains(got[0], "--cookies") || !strings.Contains(got[1], "--cookies-from-browser chrome") {
		t.Errorf("yt-dlp calls = %q: want the first without cookies, the second with them", got)
	}

	// A cookies file takes the --cookies form and goes into downloads too.
	yt, _ = ageGate(t, YouTubeConfig{CookiesFile: "/tmp/c.txt"})
	if args := strings.Join(yt.ytdlpArgs(true), " "); !strings.Contains(args, "--cookies /tmp/c.txt") {
		t.Errorf("args = %q", args)
	}
	if args := strings.Join(yt.ytdlpArgs(false), " "); strings.Contains(args, "cookies") {
		t.Errorf("args without auth = %q", args)
	}
}

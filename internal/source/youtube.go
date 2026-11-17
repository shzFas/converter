package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// YouTubeConfig tells where the helper programs live.
type YouTubeConfig struct {
	YtDlp   string // yt-dlp binary
	FFmpeg  string // ffmpeg binary
	FFprobe string // ffprobe binary
	Proxy   string // outgoing proxy for yt-dlp, empty for none
	// A signed-in YouTube account for age-restricted videos: a cookies.txt
	// file, or the name of a browser to read the cookies from ("chrome",
	// "firefox:Profile"). Both empty means such videos are refused.
	CookiesFile    string
	CookiesBrowser string
	Logger         *slog.Logger
}

// YouTube downloads from YouTube with yt-dlp and ffmpeg.
type YouTube struct {
	cfg   YouTubeConfig
	log   *slog.Logger
	stall time.Duration // see stallTimeout
}

func NewYouTube(cfg YouTubeConfig) *YouTube {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &YouTube{cfg: cfg, log: cfg.Logger, stall: stallTimeout}
}

func (y *YouTube) Name() string { return "youtube" }

func (y *YouTube) Match(rawURL string) (Link, error) { return matchYouTube(rawURL) }

const probeTimeout = 45 * time.Second

// Format selectors. H.264 video and AAC audio play in Telegram as is; the
// last alternatives take whatever exists and the result is re-encoded.
const (
	selWholeH264  = "bv*[vcodec^=avc1]+ba[ext=m4a]/b[vcodec^=avc1]/bv*+ba/b"
	selWholeAny   = "bv*+ba[ext=m4a]/bv*+ba/b"
	selWholeAudio = "ba/b"
	// Fragments are read by ffmpeg straight from the network. HLS formats go
	// first: ffmpeg fetches only the segments it needs and is not throttled.
	selPartH264  = "bv*[vcodec^=avc1][protocol^=m3u8]+ba[protocol^=m3u8]/" + selWholeH264
	selPartAny   = "bv*[protocol^=m3u8]+ba[protocol^=m3u8]/" + selWholeAny
	selPartAudio = "ba[protocol^=m3u8]/ba/b"
)

// hasAccount reports whether a signed-in account is configured.
func (y *YouTube) hasAccount() bool {
	return y.cfg.CookiesFile != "" || y.cfg.CookiesBrowser != ""
}

// ytdlpArgs are the options common to every yt-dlp call. The account is
// used only when asked for: the fewer requests go out under it, the lower
// the risk that YouTube bans it.
func (y *YouTube) ytdlpArgs(auth bool) []string {
	args := []string{"--no-playlist", "--socket-timeout", "30", "--retries", "3"}
	switch {
	case !auth:
	case y.cfg.CookiesFile != "":
		args = append(args, "--cookies", y.cfg.CookiesFile)
	case y.cfg.CookiesBrowser != "":
		args = append(args, "--cookies-from-browser", y.cfg.CookiesBrowser)
	}
	if y.cfg.Proxy != "" {
		args = append(args, "--proxy", y.cfg.Proxy)
	}
	if strings.ContainsRune(y.cfg.FFmpeg, filepath.Separator) {
		args = append(args, "--ffmpeg-location", y.cfg.FFmpeg)
	}
	return args
}

// Probe fetches metadata with `yt-dlp -J`. An age-restricted video is asked
// for again under the configured account, if there is one.
func (y *YouTube) Probe(ctx context.Context, link Link) (*Info, error) {
	info, err := y.probe(ctx, link, false)
	if KindOf(err) != KindAgeRestricted || !y.hasAccount() {
		return info, err
	}
	y.log.Info("age-restricted video, retrying with the account", "video", link.ID)
	if info, err = y.probe(ctx, link, true); err != nil {
		return nil, err
	}
	info.Auth = true
	return info, nil
}

func (y *YouTube) probe(ctx context.Context, link Link, auth bool) (*Info, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	args := append(y.ytdlpArgs(auth), "-J", "--", link.URL)
	out, stderr, err := output(probeCtx, y.cfg.YtDlp, args)
	switch {
	case err == nil:
		return parseYouTubeInfo(out)
	case ctx.Err() == nil && probeCtx.Err() != nil:
		return nil, &Error{Kind: KindBlocked, Detail: fmt.Sprintf("yt-dlp did not answer in %s: %s", probeTimeout, stderr)}
	default:
		return nil, ytdlpError(ctx, err, stderr)
	}
}

// Download produces the requested file in req.Dir.
func (y *YouTube) Download(ctx context.Context, req Request, progress func(Progress)) (*Result, error) {
	if progress == nil {
		progress = func(Progress) {}
	}
	if req.Info == nil {
		return nil, errors.New("source: request without info")
	}
	quality, ok := req.Info.Quality(req.Tier)
	if req.Format == MP4 && !ok {
		return nil, errors.New("source: no video stream")
	}

	var media string
	var err error
	if req.Whole() {
		media, err = y.downloadWhole(ctx, req, quality, progress)
	} else {
		media, err = y.downloadPart(ctx, req, quality, progress)
	}
	if err != nil {
		return nil, err
	}

	if req.Format == MP4 {
		if media, err = y.ensureMP4(ctx, media, req.Dir, progress); err != nil {
			return nil, err
		}
	}

	probe, err := y.ffprobe(ctx, media)
	if err != nil {
		return nil, err
	}
	stat, err := os.Stat(media)
	if err != nil {
		return nil, err
	}
	res := &Result{Path: media, Size: stat.Size(), Duration: probe.duration()}
	if v := probe.video(); v != nil {
		res.Width, res.Height = v.Width, v.Height
	}
	res.Thumb = y.makeThumb(ctx, req, res)
	return res, nil
}

// downloadWhole lets yt-dlp download and post-process the entire video.
func (y *YouTube) downloadWhole(ctx context.Context, req Request, q Quality, progress func(Progress)) (string, error) {
	args := append(y.ytdlpArgs(req.Info.Auth),
		"--newline",
		"--progress-template", "download:"+progressPrefix+" %(progress.status)s %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
		"--no-mtime",
		"--write-thumbnail", "--convert-thumbnails", "jpg",
		"-o", filepath.Join(req.Dir, "media.%(ext)s"),
	)

	expected := int64(0)
	if req.Format == MP3 {
		args = append(args,
			"-f", selWholeAudio,
			"-x", "--audio-format", "mp3", "--audio-quality", strconv.Itoa(MP3Bitrate/1000)+"K",
			"--embed-metadata", "--embed-thumbnail",
			// The performer is the channel name; the description is not worth carrying.
			"--parse-metadata", "%(channel,uploader)s:%(meta_artist)s",
			"--parse-metadata", ":(?P<meta_synopsis>)",
			"--parse-metadata", ":(?P<meta_description>)",
			"--parse-metadata", ":(?P<meta_comment>)",
			"--parse-metadata", ":(?P<meta_purl>)",
		)
	} else {
		selector := selWholeH264
		if q.Recode {
			selector = selWholeAny
		}
		args = append(args,
			"-f", selector,
			"-S", "res:"+strconv.Itoa(req.Tier),
			"--merge-output-format", "mp4",
		)
		expected = q.VideoBytes + req.Info.AudioBytes
	}
	args = append(args, "--", req.Link.URL)

	tracker := &downloadTracker{expected: expected, report: progress}
	stderr, err := run(ctx, y.cfg.YtDlp, args, tracker.line)
	if err != nil {
		return "", ytdlpError(ctx, err, stderr)
	}

	want := filepath.Join(req.Dir, "media."+string(req.Format))
	if _, err := os.Stat(want); err == nil {
		return want, nil
	}
	// Without an MP4-compatible pair yt-dlp may leave another container.
	if req.Format == MP4 {
		for _, ext := range []string{"mkv", "webm", "mov"} {
			other := filepath.Join(req.Dir, "media."+ext)
			if _, err := os.Stat(other); err == nil {
				return other, nil
			}
		}
	}
	return "", &Error{Kind: KindUnknown, Detail: "yt-dlp finished without an output file: " + stderr}
}

// Reading a fragment over the network sometimes hangs: the CDN accepts the
// connection and then sends nothing for minutes. A hung attempt is killed
// and repeated with freshly resolved URLs.
const (
	partAttempts = 3
	stallTimeout = 20 * time.Second
	// A result this much shorter than requested has lost a piece on the way.
	partTolerance = 2 * time.Second
)

var errStalled = errors.New("source: download stalled")

// downloadPart cuts a fragment, retrying when the network lets it down.
func (y *YouTube) downloadPart(ctx context.Context, req Request, q Quality, progress func(Progress)) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= partAttempts; attempt++ {
		dst, err := y.cutOnce(ctx, req, q, progress)
		if err == nil {
			return dst, nil
		}
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		// Only vague failures are worth another try: a private or deleted
		// video stays that way.
		if kind := KindOf(err); kind != KindUnknown && kind != KindBlocked {
			return "", err
		}
		lastErr = err
		y.log.Warn("fragment attempt failed", "video", req.Link.ID, "attempt", attempt, "err", err)
	}
	return "", lastErr
}

// cutOnce makes one attempt at a fragment: yt-dlp only resolves the stream
// URLs and ffmpeg reads the needed part of them directly.
//
// `yt-dlp --download-sections` runs the same ffmpeg in one call, but it
// reports no progress for fragments and waits on a hung connection
// indefinitely; driving ffmpeg from here gives both a percentage and the
// stall watchdog.
func (y *YouTube) cutOnce(ctx context.Context, req Request, q Quality, progress func(Progress)) (string, error) {
	selector := selPartAudio
	switch {
	case req.Format == MP4 && q.Recode:
		selector = selPartAny
	case req.Format == MP4:
		selector = selPartH264
	}
	args := append(y.ytdlpArgs(req.Info.Auth), "-J", "-f", selector)
	if req.Format == MP4 {
		args = append(args, "-S", "res:"+strconv.Itoa(req.Tier))
	}
	args = append(args, "--", req.Link.URL)

	resolveCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	out, stderr, err := output(resolveCtx, y.cfg.YtDlp, args)
	cancel()
	if err != nil {
		return "", ytdlpError(ctx, err, stderr)
	}
	var resolved ytInfo
	if err := json.Unmarshal(out, &resolved); err != nil {
		return "", fmt.Errorf("decode yt-dlp output: %w", err)
	}
	streams := resolved.RequestedFormats
	if len(streams) == 0 {
		streams = []ytFormat{resolved.ytFormat}
	}

	ids := make([]string, len(streams))
	for i, s := range streams {
		ids[i] = s.FormatID + "/" + s.Protocol
	}
	y.log.Debug("streams resolved", "video", req.Link.ID, "formats", ids)

	length := req.End - req.Start
	ff := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin"}
	videoIn, audioIn := -1, -1
	copyVideo, copyAudio := false, false
	for i, s := range streams {
		if u, err := url.Parse(s.URL); err != nil || u.Scheme != "https" {
			return "", &Error{Kind: KindUnknown, Detail: "yt-dlp returned an unexpected stream URL"}
		}
		if headers := headerLines(s.HTTPHeaders); headers != "" {
			ff = append(ff, "-headers", headers)
		}
		// ffmpeg only speaks HTTP proxies; with a SOCKS one it goes direct.
		if strings.HasPrefix(y.cfg.Proxy, "http://") {
			ff = append(ff, "-http_proxy", y.cfg.Proxy)
		}
		ff = append(ff,
			"-ss", seconds(req.Start),
			"-t", seconds(length),
			"-i", s.URL,
		)
		if s.VCodec != "" && s.VCodec != "none" && videoIn < 0 {
			videoIn, copyVideo = i, s.isH264()
		}
		// HLS audio-only formats come without acodec, so "not video" is the test.
		if (s.VCodec == "" || s.VCodec == "none" || len(streams) == 1) && audioIn < 0 {
			audioIn = i
			copyAudio = s.ACodec == "" || strings.HasPrefix(s.ACodec, "mp4a")
		}
	}
	if audioIn < 0 && videoIn < 0 {
		return "", &Error{Kind: KindUnknown, Detail: "yt-dlp returned no streams"}
	}

	var dst string
	if req.Format == MP3 {
		if audioIn < 0 {
			audioIn = videoIn
		}
		dst = filepath.Join(req.Dir, "media.mp3")
		cover := y.fetchCover(ctx, req)
		if cover != "" {
			ff = append(ff, "-i", cover)
		}
		ff = append(ff, "-map", fmt.Sprintf("%d:a:0", audioIn))
		if cover != "" {
			ff = append(ff,
				"-map", fmt.Sprintf("%d:v:0", len(streams)),
				"-c:v", "copy", "-disposition:v", "attached_pic",
				"-metadata:s:v", "title=Album cover", "-metadata:s:v", "comment=Cover (front)",
			)
		}
		ff = append(ff,
			"-c:a", "libmp3lame", "-b:a", strconv.Itoa(MP3Bitrate/1000)+"k",
			"-id3v2_version", "3",
			"-metadata", "title="+req.Info.Title,
			"-metadata", "artist="+req.Info.Channel,
		)
	} else {
		if videoIn < 0 {
			return "", &Error{Kind: KindUnknown, Detail: "yt-dlp returned no video stream"}
		}
		dst = filepath.Join(req.Dir, "media.mp4")
		ff = append(ff, "-map", fmt.Sprintf("%d:v:0", videoIn))
		if audioIn >= 0 {
			ff = append(ff, "-map", fmt.Sprintf("%d:a:0?", audioIn))
		}
		// A precise cut re-encodes everything to put a keyframe exactly at the
		// start; a fast one copies the streams and begins at the nearest
		// keyframe before it.
		if req.Precise || !copyVideo {
			ff = append(ff, h264Args...)
		} else {
			ff = append(ff, "-c:v", "copy")
		}
		if req.Precise || !copyAudio {
			ff = append(ff, aacArgs...)
		} else {
			ff = append(ff, "-c:a", "copy")
		}
		ff = append(ff, "-movflags", "+faststart")
	}
	ff = append(ff, "-progress", "pipe:1", "-nostats", dst)

	// The watchdog fires when the output position has not moved for a while.
	attemptCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	watchdog := time.AfterFunc(y.stall, func() { stop(errStalled) })
	defer watchdog.Stop()

	report := ffmpegProgress(length, StageDownload, progress)
	position := int64(-1)
	stderr, err = run(attemptCtx, y.cfg.FFmpeg, ff, func(line string) {
		if value, ok := strings.CutPrefix(line, "out_time_us="); ok {
			if us := atoi(value); us > position {
				position = us
				watchdog.Reset(y.stall)
			}
		}
		report(line)
	})
	switch {
	case ctx.Err() != nil:
		return "", context.Cause(ctx)
	case errors.Is(err, errStalled):
		return "", &Error{Kind: KindBlocked, Detail: fmt.Sprintf("ffmpeg made no progress for %s", y.stall)}
	case err != nil:
		return "", ffmpegError(ctx, err, stderr)
	}

	// ffmpeg skips an HLS segment it could not fetch and still exits with 0,
	// so make sure nothing is missing.
	probe, err := y.ffprobe(ctx, dst)
	if err != nil {
		return "", err
	}
	if got := probe.duration(); got < length-partTolerance {
		return "", &Error{Kind: KindUnknown, Detail: fmt.Sprintf("fragment is %s long instead of %s", got, length)}
	}
	return dst, nil
}

var (
	h264Args = []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "21", "-pix_fmt", "yuv420p"}
	aacArgs  = []string{"-c:a", "aac", "-b:a", "160k"}
)

// ensureMP4 re-encodes whatever is not H.264 + AAC in an MP4 container.
// In the usual case the file is already right and is returned untouched.
func (y *YouTube) ensureMP4(ctx context.Context, media, dir string, progress func(Progress)) (string, error) {
	probe, err := y.ffprobe(ctx, media)
	if err != nil {
		return "", err
	}
	video, audio := probe.video(), probe.audio()
	if video == nil {
		return "", &Error{Kind: KindUnknown, Detail: "downloaded file has no video stream"}
	}
	videoOK := video.CodecName == "h264"
	audioOK := audio == nil || audio.CodecName == "aac"
	if videoOK && audioOK && strings.EqualFold(filepath.Ext(media), ".mp4") {
		return media, nil
	}

	y.log.Info("re-encoding to H.264/AAC", "video", video.CodecName, "container", filepath.Ext(media))
	progress(Progress{Stage: StageConvert, Percent: 0})
	dst := filepath.Join(dir, "final.mp4")
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", media, "-map", "0:v:0", "-map", "0:a:0?"}
	if videoOK {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, h264Args...)
	}
	if audioOK {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, aacArgs...)
	}
	args = append(args, "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", dst)

	stderr, err := run(ctx, y.cfg.FFmpeg, args, ffmpegProgress(probe.duration(), StageConvert, progress))
	if err != nil {
		return "", ffmpegError(ctx, err, stderr)
	}
	_ = os.Remove(media)
	return dst, nil
}

// fetchCover saves the video thumbnail as JPEG for embedding into an MP3.
// A missing cover is not an error.
func (y *YouTube) fetchCover(ctx context.Context, req Request) string {
	if !strings.HasPrefix(req.Info.Thumbnail, "https://") {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	dst := filepath.Join(req.Dir, "media.jpg")
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", req.Info.Thumbnail, "-frames:v", "1", "-q:v", "3", dst}
	if stderr, err := run(ctx, y.cfg.FFmpeg, args, nil); err != nil {
		y.log.Warn("cover download failed", "video", req.Link.ID, "err", err, "stderr", stderr)
		return ""
	}
	return dst
}

// makeThumb prepares the small preview Telegram shows for the file: the
// video's own thumbnail, or a frame of the result when a fragment was cut.
func (y *YouTube) makeThumb(ctx context.Context, req Request, res *Result) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// Telegram wants a JPEG of at most 320x320 pixels and 200 KB.
	const scale = "scale=320:320:force_original_aspect_ratio=decrease"
	dst := filepath.Join(req.Dir, "thumb.jpg")
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostdin"}

	cover := filepath.Join(req.Dir, "media.jpg")
	_, coverErr := os.Stat(cover)
	switch {
	case coverErr == nil && (req.Whole() || req.Format == MP3):
		args = append(args, "-i", cover)
	case req.Format == MP4:
		// A frame a bit into the fragment: the very first one is often black.
		at := min(res.Duration/10, 3*time.Second)
		args = append(args, "-ss", seconds(at), "-i", res.Path)
	default:
		return ""
	}
	args = append(args, "-frames:v", "1", "-vf", scale, "-q:v", "5", dst)

	if stderr, err := run(ctx, y.cfg.FFmpeg, args, nil); err != nil {
		y.log.Warn("thumbnail failed", "video", req.Link.ID, "err", err, "stderr", stderr)
		return ""
	}
	return dst
}

// ffprobeOutput is the part of `ffprobe -print_format json` the bot uses.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type ffprobeStream struct {
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

func (p *ffprobeOutput) stream(kind string) *ffprobeStream {
	for i := range p.Streams {
		// An embedded cover is reported as a video stream too.
		if p.Streams[i].CodecType == kind && p.Streams[i].Disposition.AttachedPic == 0 {
			return &p.Streams[i]
		}
	}
	return nil
}

func (p *ffprobeOutput) video() *ffprobeStream { return p.stream("video") }
func (p *ffprobeOutput) audio() *ffprobeStream { return p.stream("audio") }

func (p *ffprobeOutput) duration() time.Duration {
	sec, _ := strconv.ParseFloat(p.Format.Duration, 64)
	return time.Duration(sec * float64(time.Second))
}

func (y *YouTube) ffprobe(ctx context.Context, path string) (*ffprobeOutput, error) {
	args := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path}
	out, stderr, err := output(ctx, y.cfg.FFprobe, args)
	if err != nil {
		return nil, ffmpegError(ctx, err, stderr)
	}
	var probe ffprobeOutput
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("decode ffprobe output: %w", err)
	}
	return &probe, nil
}

const progressPrefix = "PROGRESS"

// downloadTracker turns yt-dlp output into overall progress. yt-dlp reports
// every file (video, then audio) separately, so finished files are summed up.
type downloadTracker struct {
	expected int64 // estimated total of all files, 0 if unknown
	finished int64 // bytes of completed files
	report   func(Progress)
}

func (t *downloadTracker) line(line string) {
	if !strings.HasPrefix(line, progressPrefix+" ") {
		// "[Merger] ...", "[ExtractAudio] ..." and friends run after the download.
		for _, pp := range []string{"[Merger]", "[ExtractAudio]", "[VideoConvertor]", "[VideoRemuxer]", "[Metadata]", "[EmbedThumbnail]", "[Fixup"} {
			if strings.HasPrefix(line, pp) {
				t.report(Progress{Stage: StageConvert, Percent: -1})
				return
			}
		}
		return
	}

	fields := strings.Fields(line)
	if len(fields) < 5 {
		return
	}
	downloaded := atoi(fields[2])
	total := atoi(fields[3])
	if total == 0 {
		total = atoi(fields[4])
	}
	if fields[1] == "finished" {
		t.finished += max(total, downloaded)
		downloaded, total = 0, 0
	}

	all := max(t.expected, t.finished+total)
	if all <= 0 {
		t.report(Progress{Stage: StageDownload, Percent: -1})
		return
	}
	percent := float64(t.finished+downloaded) / float64(all) * 100
	t.report(Progress{Stage: StageDownload, Percent: min(percent, 99)})
}

// ffmpegProgress parses `-progress pipe:1` output against the expected length.
func ffmpegProgress(length time.Duration, stage Stage, report func(Progress)) func(string) {
	return func(line string) {
		value, ok := strings.CutPrefix(line, "out_time_us=")
		if !ok || length <= 0 {
			return
		}
		us := atoi(value)
		if us < 0 {
			return
		}
		percent := float64(us) / float64(length.Microseconds()) * 100
		report(Progress{Stage: stage, Percent: min(percent, 99)})
	}
}

func atoi(s string) int64 {
	// yt-dlp prints floats for estimates and "NA" for unknown values.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}

func headerLines(headers map[string]string) string {
	var b strings.Builder
	// User-Agent is enough; map order is random and the rest changes nothing.
	if ua := headers["User-Agent"]; ua != "" && !strings.ContainsAny(ua, "\r\n") {
		b.WriteString("User-Agent: " + ua + "\r\n")
	}
	return b.String()
}

func ffmpegError(ctx context.Context, err error, stderr string) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return &Error{Kind: KindUnknown, Detail: fmt.Sprintf("ffmpeg: %v: %s", err, stderr)}
}

// ytdlpError classifies a yt-dlp failure by its stderr.
func ytdlpError(ctx context.Context, err error, stderr string) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return &Error{Kind: classify(stderr), Detail: fmt.Sprintf("yt-dlp: %v: %s", err, stderr)}
}

// classify maps yt-dlp's error text to a failure kind. The order matters:
// "Private video. Sign in if..." is private, not age-restricted.
func classify(stderr string) Kind {
	text := strings.ToLower(strings.ReplaceAll(stderr, "’", "'"))
	contains := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("requested format"):
		return KindUnknown
	case contains("private video", "video is private"):
		return KindPrivate
	case contains("confirm your age", "age-restricted", "age restricted", "inappropriate for some users"):
		return KindAgeRestricted
	case contains("members-only", "members only", "join this channel", "channel's members"):
		return KindMembersOnly
	case contains("in your country", "from your location", "geo-restricted", "geo restricted"):
		return KindGeo
	case contains("live event", "premieres in", "premiere will begin"):
		return KindLive
	case contains("not a bot", "http error 429", "too many requests", "rate-limited", "rate limited", "http error 403"):
		return KindBlocked
	case contains("video unavailable", "is unavailable", "not available", "has been removed",
		"no longer available", "has been terminated", "does not exist", "incomplete youtube id", "truncated"):
		return KindUnavailable
	}
	return KindUnknown
}

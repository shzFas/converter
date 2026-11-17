// Command ytcheck downloads one video without Telegram. It is the "stage 0"
// check from the spec: run it on the machine where the bot will live to see
// whether YouTube lets that machine download at all.
//
//	go run ./cmd/ytcheck https://youtu.be/jNQXAC9IVRw
//	go run ./cmd/ytcheck -format mp3 -cut 0:05-0:15 https://youtu.be/jNQXAC9IVRw
//	go run ./cmd/ytcheck -format txt https://youtu.be/jNQXAC9IVRw
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"converter/internal/source"
	"converter/internal/timecode"
	"converter/internal/transcribe"
)

func main() {
	format := flag.String("format", "mp4", "mp4, mp3 or txt (speech to text with whisper.cpp)")
	tier := flag.Int("q", 720, "video quality: 360, 720 or 1080")
	cut := flag.String("cut", "", "fragment, e.g. 1:30-2:45")
	fast := flag.Bool("fast", false, "cut without re-encoding (boundaries may shift)")
	out := flag.String("out", "", "output directory (default: a new temporary one)")
	verbose := flag.Bool("v", false, "log what the downloader does")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ytcheck [flags] <youtube link>")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := check(ctx, log, flag.Arg(0), source.Format(*format), *tier, *cut, *fast, *out); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func check(ctx context.Context, log *slog.Logger, rawURL string, format source.Format, tier int, cut string, fast bool, dir string) error {
	yt := source.NewYouTube(source.YouTubeConfig{
		YtDlp:          env("YTDLP_PATH", "yt-dlp"),
		FFmpeg:         env("FFMPEG_PATH", "ffmpeg"),
		FFprobe:        env("FFPROBE_PATH", "ffprobe"),
		Proxy:          os.Getenv("PROXY_URL"),
		CookiesFile:    os.Getenv("YTDLP_COOKIES_FILE"),
		CookiesBrowser: os.Getenv("YTDLP_COOKIES_BROWSER"),
		Logger:         log,
	})

	link, err := yt.Match(rawURL)
	if err != nil {
		return err
	}

	started := time.Now()
	info, err := yt.Probe(ctx, link)
	if err != nil {
		return err
	}
	fmt.Printf("%s — %s, %s (probe %s)\n", info.Title, info.Channel, timecode.Format(info.Duration), time.Since(started).Round(time.Millisecond))
	for _, q := range info.Qualities {
		fmt.Printf("  %-6s ≈ %5.1f MB whole  recode=%v\n", q.Label, mb(info.EstimateSize(source.MP4, q.Tier, info.Duration)), q.Recode)
	}
	fmt.Printf("  mp3    ≈ %5.1f MB whole\n", mb(info.EstimateSize(source.MP3, 0, info.Duration)))

	text := format == "txt"
	if text {
		format = source.MP3
	}
	req := source.Request{Link: link, Info: info, Format: format, Tier: tier}
	length := info.Duration
	if cut != "" {
		r, err := timecode.Parse(cut)
		if err != nil {
			return err
		}
		start, end, whole, err := r.Resolve(info.Duration)
		if err != nil {
			return err
		}
		if !whole {
			req.Start, req.End, req.Precise = start, end, !fast
			length = end - start
		}
	}

	if dir == "" {
		if dir, err = os.MkdirTemp("", "ytcheck-"); err != nil {
			return err
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	req.Dir = dir

	started = time.Now()
	last := -1
	res, err := yt.Download(ctx, req, func(p source.Progress) {
		if step := int(p.Percent) / 10; p.Percent >= 0 && step != last {
			last = step
			fmt.Printf("  stage %d: %.0f%%\n", p.Stage, p.Percent)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("OK %s\n  %.1f MB (estimate %.1f MB), %s, %dx%d, thumb=%q, took %s\n",
		res.Path, mb(res.Size), mb(info.EstimateSize(format, tier, length)),
		timecode.Format(res.Duration), res.Width, res.Height, res.Thumb, time.Since(started).Round(time.Millisecond))
	if text {
		return transcribeCheck(ctx, res.Path, dir, req.Start)
	}
	return nil
}

// transcribeCheck runs the same whisper.cpp setup as the bot and prints the text.
func transcribeCheck(ctx context.Context, audio, dir string, offset time.Duration) error {
	w := transcribe.NewWhisper(transcribe.WhisperConfig{
		Binary:   env("WHISPER_PATH", "whisper-cli"),
		Model:    env("WHISPER_MODEL", "data/models/ggml-large-v3-turbo-q5_0.bin"),
		VADModel: env("WHISPER_VAD_MODEL", "data/models/ggml-silero-v5.1.2.bin"),
		Language: env("WHISPER_LANGUAGE", "auto"),
		FFmpeg:   env("FFMPEG_PATH", "ffmpeg"),
	})
	if err := w.Check(); err != nil {
		return err
	}
	started := time.Now()
	last := -1
	res, err := w.Transcribe(ctx, audio, dir, func(p float64) {
		if step := int(p) / 10; step != last {
			last = step
			fmt.Printf("  whisper: %.0f%%\n", p)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("OK transcript: language %q, %d phrases, took %s\n\n%s\n", res.Language, len(res.Segments),
		time.Since(started).Round(time.Millisecond), res.Timed(offset))
	return nil
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

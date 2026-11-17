// Command bot is the Telegram bot that turns a YouTube link into MP4 or MP3.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"converter/internal/bot"
	"converter/internal/queue"
	"converter/internal/source"
	"converter/internal/store"
	"converter/internal/transcribe"
)

// shutdownGrace is how long running jobs get to finish after SIGTERM.
const shutdownGrace = 60 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Бот не запущен:")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := loadDotEnv(".env"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	for _, tool := range []*string{&cfg.YtDlp, &cfg.FFmpeg, &cfg.FFprobe} {
		path, err := exec.LookPath(*tool)
		if err != nil {
			return fmt.Errorf("не найдена программа %q — установите её (на macOS: brew install yt-dlp ffmpeg deno) или укажите путь в .env", *tool)
		}
		*tool = path
	}

	// Whatever a previous run has left behind is garbage by definition.
	if err := cleanTmp(cfg.TmpDir); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "store.json"))
	if err != nil {
		return err
	}

	jobs := queue.New(queue.Config{
		Workers:   cfg.Workers,
		Size:      cfg.QueueSize,
		PerUser:   cfg.UserActiveJobs,
		TaskLimit: cfg.JobTimeout,
	})
	youtube := source.NewYouTube(source.YouTubeConfig{
		YtDlp:          cfg.YtDlp,
		FFmpeg:         cfg.FFmpeg,
		FFprobe:        cfg.FFprobe,
		Proxy:          cfg.ProxyURL,
		CookiesFile:    cfg.CookiesFile,
		CookiesBrowser: cfg.CookiesBrowser,
		Logger:         log,
	})
	transcriber := newTranscriber(cfg, log)
	app := bot.New(bot.Config{
		DailyQuota:    cfg.UserDailyQuota,
		MaxFileBytes:  int64(cfg.MaxFileMB) << 20,
		MaxDuration:   cfg.MaxDuration,
		PreciseCutMax: cfg.PreciseCutMax,
		DefaultTier:   720,
		JobTimeout:    cfg.JobTimeout,
		InputTimeout:  5 * time.Minute,
		StatusEvery:   3 * time.Second,
		TmpDir:        cfg.TmpDir,
		Admins:        cfg.Admins,
		Allowed:       cfg.Allowed,
		Transcriber:   transcriber,
	}, []source.Source{youtube}, jobs, st, log)

	// No overall client timeout: a 48 MB upload on a slow line takes minutes
	// and is bounded by the job timeout instead. A dead connection is caught
	// by the header timeout, which is longer than one long-polling round.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	options := []tg.Option{
		tg.WithDefaultHandler(app.Handler),
		// One synchronous worker hands updates over in the order Telegram
		// sent them; app.Handler returns at once, so nobody waits.
		tg.WithWorkers(1),
		tg.WithNotAsyncHandlers(),
		tg.WithHTTPClient(time.Minute, &http.Client{Transport: transport}),
		tg.WithAllowedUpdates(tg.AllowedUpdates{"message", "callback_query"}),
		tg.WithErrorsHandler(func(err error) { log.Warn("telegram", "err", err) }),
	}
	if cfg.BotAPIURL != "" {
		options = append(options, tg.WithServerURL(cfg.BotAPIURL))
	}
	client, err := tg.New(cfg.Token, options...)
	if err != nil {
		if errors.Is(err, tg.ErrorUnauthorized) || errors.Is(err, tg.ErrorNotFound) {
			return errors.New("Telegram не принял BOT_TOKEN — проверьте токен в .env")
		}
		return fmt.Errorf("нет связи с Telegram: %w", err)
	}
	app.Attach(client)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	setCommands(ctx, client, log)
	access := "everyone"
	if len(cfg.Allowed) > 0 {
		access = "allow-list"
	}
	log.Info("bot started", "workers", cfg.Workers, "queue", cfg.QueueSize, "max_file_mb", cfg.MaxFileMB,
		"access", access, "admins", len(cfg.Admins), "account", cfg.CookiesFile != "" || cfg.CookiesBrowser != "", "yt_dlp", cfg.YtDlp, "ffmpeg", cfg.FFmpeg)

	client.Start(ctx) // long polling; returns when a signal arrives
	stop()            // a second Ctrl+C now kills the process the usual way

	log.Info("shutting down", "grace", shutdownGrace.String())
	graceCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	app.Shutdown(graceCtx)

	if err := cleanTmp(cfg.TmpDir); err != nil {
		log.Warn("cleaning tmp", "err", err)
	}
	log.Info("bye")
	return nil
}

// newTranscriber sets up whisper.cpp, or returns nil — and the bot hides the
// text option — when the program or the model is missing.
func newTranscriber(cfg config, log *slog.Logger) transcribe.Transcriber {
	if cfg.WhisperModel == "" {
		log.Info("transcription is off: WHISPER_MODEL is empty")
		return nil
	}
	vad := cfg.WhisperVAD
	if vad != "" {
		if _, err := os.Stat(vad); err != nil {
			log.Warn("VAD model not found, transcribing without it", "path", vad)
			vad = ""
		}
	}
	w := transcribe.NewWhisper(transcribe.WhisperConfig{
		Binary:   cfg.Whisper,
		Model:    cfg.WhisperModel,
		VADModel: vad,
		Language: cfg.WhisperLanguage,
		Threads:  cfg.WhisperThreads,
		FFmpeg:   cfg.FFmpeg,
	})
	if err := w.Check(); err != nil {
		log.Warn("transcription is off", "err", err)
		return nil
	}
	log.Info("transcription is on", "model", filepath.Base(cfg.WhisperModel), "vad", vad != "")
	return w
}

// setCommands fills the command menu of the chat. Failing to do so is not
// a reason to stay down.
func setCommands(ctx context.Context, client *tg.Bot, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := client.SetMyCommands(ctx, &tg.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "start", Description: "Как пользоваться"},
		{Command: "help", Description: "Таймкоды и лимиты"},
		{Command: "mp3", Description: "Сразу MP3: /mp3 ссылка [таймкоды]"},
		{Command: "mp4", Description: "Сразу MP4 720p: /mp4 ссылка [таймкоды]"},
		{Command: "text", Description: "Расшифровка речи: /text ссылка [таймкоды]"},
		{Command: "cancel", Description: "Отменить текущую задачу"},
	}})
	if err != nil {
		log.Warn("set commands", "err", err)
	}
}

// cleanTmp creates the temporary directory and removes the job directories
// left in it.
func cleanTmp(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stale, err := filepath.Glob(filepath.Join(dir, "job-*"))
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

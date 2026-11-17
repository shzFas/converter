package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// config is everything the bot reads from the environment. The defaults are
// the starting limits from the spec.
type config struct {
	Token     string // BOT_TOKEN
	BotAPIURL string // BOT_API_URL, a local Bot API server; empty for the cloud one

	Workers        int           // WORKERS
	QueueSize      int           // QUEUE_SIZE
	UserActiveJobs int           // USER_ACTIVE_JOBS
	UserDailyQuota int           // USER_DAILY_QUOTA
	MaxFileMB      int           // MAX_FILE_MB
	MaxDuration    time.Duration // MAX_DURATION_MIN
	JobTimeout     time.Duration // JOB_TIMEOUT
	PreciseCutMax  time.Duration // PRECISE_CUT_MAX_MIN

	Admins  []int64 // ADMIN_IDS
	Allowed []int64 // ALLOWED_USERS

	ProxyURL       string // PROXY_URL
	CookiesFile    string // YTDLP_COOKIES_FILE
	CookiesBrowser string // YTDLP_COOKIES_BROWSER
	DataDir        string // DATA_DIR
	TmpDir         string // TMP_DIR
	YtDlp          string // YTDLP_PATH
	FFmpeg         string // FFMPEG_PATH
	FFprobe        string // FFPROBE_PATH

	Whisper         string // WHISPER_PATH
	WhisperModel    string // WHISPER_MODEL, empty turns transcription off
	WhisperVAD      string // WHISPER_VAD_MODEL, optional
	WhisperLanguage string // WHISPER_LANGUAGE
	WhisperThreads  int    // WHISPER_THREADS

	LogLevel slog.Level // LOG_LEVEL
}

func loadConfig() (config, error) {
	var errs []error
	integer := func(key string, fallback, min int) int {
		raw := os.Getenv(key)
		if raw == "" {
			return fallback
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < min {
			errs = append(errs, fmt.Errorf("%s=%q: нужно целое число не меньше %d", key, raw, min))
			return fallback
		}
		return n
	}
	// minutes accepts a bare number of minutes or a Go duration such as "90s".
	minutes := func(key string, fallback time.Duration) time.Duration {
		raw := os.Getenv(key)
		if raw == "" {
			return fallback
		}
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		errs = append(errs, fmt.Errorf("%s=%q: нужно число минут или длительность вида 10m", key, raw))
		return fallback
	}
	ids := func(key string) []int64 {
		var list []int64
		for _, part := range strings.FieldsFunc(os.Getenv(key), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %q — не числовой ID пользователя", key, part))
				continue
			}
			list = append(list, id)
		}
		return list
	}
	text := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}

	cfg := config{
		Token:     os.Getenv("BOT_TOKEN"),
		BotAPIURL: strings.TrimRight(os.Getenv("BOT_API_URL"), "/"),

		Workers:        integer("WORKERS", 2, 1),
		QueueSize:      integer("QUEUE_SIZE", 20, 1),
		UserActiveJobs: integer("USER_ACTIVE_JOBS", 1, 1),
		UserDailyQuota: integer("USER_DAILY_QUOTA", 20, 0),
		MaxFileMB:      integer("MAX_FILE_MB", 48, 1),
		MaxDuration:    minutes("MAX_DURATION_MIN", 60*time.Minute),
		JobTimeout:     minutes("JOB_TIMEOUT", 10*time.Minute),
		PreciseCutMax:  minutes("PRECISE_CUT_MAX_MIN", 10*time.Minute),

		Admins:  ids("ADMIN_IDS"),
		Allowed: ids("ALLOWED_USERS"),

		ProxyURL:       os.Getenv("PROXY_URL"),
		CookiesFile:    os.Getenv("YTDLP_COOKIES_FILE"),
		CookiesBrowser: os.Getenv("YTDLP_COOKIES_BROWSER"),
		DataDir:        text("DATA_DIR", "data"),
		TmpDir:         text("TMP_DIR", "tmp"),
		YtDlp:          text("YTDLP_PATH", "yt-dlp"),
		FFmpeg:         text("FFMPEG_PATH", "ffmpeg"),
		FFprobe:        text("FFPROBE_PATH", "ffprobe"),

		Whisper:         text("WHISPER_PATH", "whisper-cli"),
		WhisperModel:    text("WHISPER_MODEL", "data/models/ggml-large-v3-turbo-q5_0.bin"),
		WhisperVAD:      text("WHISPER_VAD_MODEL", "data/models/ggml-silero-v5.1.2.bin"),
		WhisperLanguage: text("WHISPER_LANGUAGE", "auto"),
		WhisperThreads:  integer("WHISPER_THREADS", 4, 1),
	}

	var err error
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL=%q: нужно debug, info, warn или error", raw))
		}
	}
	if cfg.CookiesFile != "" {
		if cfg.CookiesFile, err = filepath.Abs(cfg.CookiesFile); err != nil {
			errs = append(errs, err)
		} else if _, err := os.Stat(cfg.CookiesFile); err != nil {
			errs = append(errs, fmt.Errorf("YTDLP_COOKIES_FILE: файл %s не найден", cfg.CookiesFile))
		}
	}
	if cfg.Token == "" {
		errs = append(errs, errors.New("BOT_TOKEN не задан: получите токен у @BotFather и запишите его в файл .env (образец — .env.example)"))
	}

	// Relative paths would break once yt-dlp and ffmpeg get them as arguments
	// from a different working directory, so make them absolute right away.
	if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
		errs = append(errs, err)
	}
	if cfg.TmpDir, err = filepath.Abs(cfg.TmpDir); err != nil {
		errs = append(errs, err)
	}
	for _, path := range []*string{&cfg.WhisperModel, &cfg.WhisperVAD} {
		if *path == "" || *path == "off" {
			*path = ""
		} else if *path, err = filepath.Abs(*path); err != nil {
			errs = append(errs, err)
		}
	}
	return cfg, errors.Join(errs...)
}

// loadDotEnv reads KEY=VALUE lines into the environment. Variables that are
// already set win, so `WORKERS=4 go run ./cmd/bot` overrides the file.
func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(text, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: ожидается строка вида KEY=VALUE", path, line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(" "+value, " #"); i >= 0 {
			// An unquoted value ends where a trailing comment starts.
			value = strings.TrimSpace((" " + value)[:i])
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

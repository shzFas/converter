package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv makes the test independent of the developer's environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"BOT_TOKEN", "BOT_API_URL", "WORKERS", "QUEUE_SIZE", "USER_ACTIVE_JOBS",
		"USER_DAILY_QUOTA", "MAX_FILE_MB", "MAX_DURATION_MIN", "JOB_TIMEOUT", "PRECISE_CUT_MAX_MIN",
		"ADMIN_IDS", "ALLOWED_USERS", "PROXY_URL", "YTDLP_COOKIES_FILE", "YTDLP_COOKIES_BROWSER", "DATA_DIR", "TMP_DIR", "LOG_LEVEL", "QUOTED", "EMPTY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestDotEnvAndConfig(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	content := `# comment
BOT_TOKEN=123:abc

WORKERS=4               # inline comment
export QUEUE_SIZE=7
JOB_TIMEOUT=90s
MAX_DURATION_MIN=30
ADMIN_IDS=1, 2,3
YTDLP_COOKIES_BROWSER=chrome
QUOTED="a # b"
EMPTY=   # nothing here
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// A variable already set in the environment wins over the file.
	t.Setenv("MAX_FILE_MB", "100")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("QUOTED"); got != "a # b" {
		t.Errorf("QUOTED = %q", got)
	}
	if got := os.Getenv("EMPTY"); got != "" {
		t.Errorf("EMPTY = %q", got)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "123:abc" || cfg.Workers != 4 || cfg.QueueSize != 7 || cfg.MaxFileMB != 100 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.JobTimeout != 90*time.Second || cfg.MaxDuration != 30*time.Minute {
		t.Errorf("durations = %v, %v", cfg.JobTimeout, cfg.MaxDuration)
	}
	if cfg.CookiesBrowser != "chrome" || cfg.CookiesFile != "" {
		t.Errorf("cookies = %q / %q", cfg.CookiesBrowser, cfg.CookiesFile)
	}
	if len(cfg.Admins) != 3 || cfg.Admins[2] != 3 {
		t.Errorf("admins = %v", cfg.Admins)
	}
	// The spec defaults apply to everything not mentioned.
	if cfg.UserActiveJobs != 1 || cfg.UserDailyQuota != 20 || cfg.PreciseCutMax != 10*time.Minute {
		t.Errorf("defaults = %+v", cfg)
	}
	if !filepath.IsAbs(cfg.DataDir) || !filepath.IsAbs(cfg.TmpDir) {
		t.Errorf("directories must be absolute: %q, %q", cfg.DataDir, cfg.TmpDir)
	}
}

func TestConfigErrors(t *testing.T) {
	clearEnv(t)
	t.Setenv("WORKERS", "zero")
	t.Setenv("ADMIN_IDS", "1,bob")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("a broken config was accepted")
	}
	for _, want := range []string{"BOT_TOKEN", "WORKERS", "bob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestExampleEnvIsValid(t *testing.T) {
	clearEnv(t)
	if err := loadDotEnv(filepath.Join("..", "..", ".env.example")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOT_TOKEN", "123:abc") // the example leaves it empty on purpose
	if _, err := loadConfig(); err != nil {
		t.Fatalf(".env.example does not load: %v", err)
	}
}

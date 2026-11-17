package bot

import (
	"fmt"
	"sync"
	"time"

	"converter/internal/source"
)

// The admin is alerted when more than alertShare of the downloader calls
// within alertWindow have failed: most likely YouTube has changed something.
const (
	alertWindow     = 15 * time.Minute
	alertShare      = 0.3
	alertMinSamples = 5
)

type outcome int

const (
	outcomeDone outcome = iota
	outcomeFailed
	outcomeCancelled
)

// stats are in-memory counters since the start of the process.
type stats struct {
	mu        sync.Mutex
	started   time.Time
	jobs      map[outcome]int
	cacheHits int
	workTime  time.Duration // total time of the finished jobs

	samples   []sample // downloader calls within alertWindow
	lastAlert time.Time
}

type sample struct {
	at     time.Time
	failed bool
}

func newStats() *stats {
	return &stats{started: time.Now(), jobs: make(map[outcome]int)}
}

func (s *stats) finished(o outcome, took time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[o]++
	if o == outcomeDone {
		s.workTime += took
	}
}

func (s *stats) cacheHit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheHits++
}

// sourceResult records a downloader call and tells whether it is time to
// alert the admin (at most once per window).
func (s *stats) sourceResult(failed bool) (failedCount, total int, alert bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.samples = append(s.samples, sample{at: now, failed: failed})
	failedCount, total = s.window(now)
	if total >= alertMinSamples && float64(failedCount)/float64(total) > alertShare && now.Sub(s.lastAlert) >= alertWindow {
		s.lastAlert = now
		alert = true
	}
	return failedCount, total, alert
}

// window drops the samples that are too old and counts the rest. The caller
// holds s.mu.
func (s *stats) window(now time.Time) (failedCount, total int) {
	fresh := s.samples[:0]
	for _, sm := range s.samples {
		if now.Sub(sm.at) < alertWindow {
			fresh = append(fresh, sm)
			if sm.failed {
				failedCount++
			}
		}
	}
	s.samples = fresh
	return failedCount, len(fresh)
}

// textStats is the answer to the admin's /stats.
func (b *Bot) textStats() string {
	s := b.stats
	s.mu.Lock()
	done, failed, cancelled := s.jobs[outcomeDone], s.jobs[outcomeFailed], s.jobs[outcomeCancelled]
	hits, uptime := s.cacheHits, time.Since(s.started).Round(time.Minute)
	average := time.Duration(0)
	if done > 0 {
		average = (s.workTime / time.Duration(done)).Round(time.Second)
	}
	failedCalls, calls := s.window(time.Now())
	s.mu.Unlock()

	load := b.queue.Stats()
	counts := b.store.Counts()
	return fmt.Sprintf("<b>Статистика</b> — с запуска прошло %s\n"+
		"Задачи: выполнено %d, ошибок %d, отменено %d, из кэша %d\n"+
		"Среднее время задачи: %s\n"+
		"Сейчас: в работе %d, в очереди %d\n"+
		"Сбои загрузчика за %s: %d из %d\n\n"+
		"Сегодня: %d задач от %d пользователей\n"+
		"Кэш file_id: %d · белый список: %d · чёрный список: %d · закрытых видео: %d\n"+
		"MP3 %d kbps · лимит файла %s",
		uptime, done, failed, cancelled, hits, average,
		load.Running, load.Waiting,
		formatMinutes(alertWindow), failedCalls, calls,
		counts.TasksToday, counts.UsersToday,
		counts.Files, counts.Allowed+len(b.cfg.Allowed), counts.Denied, counts.BlockedVideos,
		source.MP3Bitrate/1000, formatSize(b.cfg.MaxFileBytes))
}

package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"converter/internal/queue"
	"converter/internal/source"
	"converter/internal/transcribe"
)

type stage int

const (
	stageQueued stage = iota
	stageDownload
	stageConvert
	stageTranscribe
	stageSend
)

var (
	// errSend marks a failure to deliver the ready file to Telegram.
	errSend = errors.New("bot: sending the file failed")
	// errTranscribe marks a failure of the speech recognizer.
	errTranscribe = errors.New("bot: transcription failed")
	// errNoSpeech means the recognizer heard nothing worth writing down.
	errNoSpeech = errors.New("bot: no speech found")
)

// tooBigError means the file came out larger than estimated and over the limit.
type tooBigError struct{ size int64 }

func (e *tooBigError) Error() string {
	return fmt.Sprintf("bot: file of %d bytes is over the limit", e.size)
}

// job is one accepted request, from the queue to the file in the chat.
type job struct {
	userID  int64
	chatID  int64
	msgID   int // status message, the former video card
	src     source.Source
	req     source.Request
	info    *source.Info
	piece   piece
	quality source.Quality
	key     string // file_id cache key
	task    *queue.Task
	ready   chan struct{} // closed when the job is fully set up

	mu      sync.Mutex
	stage   stage
	percent float64
	worked  bool // the download has started

	stopUpdates chan struct{}
	updatesDone chan struct{}
}

func (j *job) set(st stage, percent float64) {
	j.mu.Lock()
	j.stage, j.percent = st, percent
	j.mu.Unlock()
}

func (j *job) state() (stage, float64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stage, j.percent
}

// cancel stops the job; it refuses once the file is on its way to the chat.
func (j *job) cancel() bool {
	if st, _ := j.state(); st == stageSend {
		return false
	}
	j.task.Cancel()
	return true
}

func (b *Bot) userJobs(userID int64) []*job {
	b.mu.Lock()
	defer b.mu.Unlock()
	var jobs []*job
	for _, j := range b.jobs {
		if j.userID == userID {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// cacheKey identifies a delivered file: video + format + quality + fragment.
func cacheKey(req source.Request, q source.Quality) string {
	return fmt.Sprintf("%s:%s|%s|%d|%d|%d", req.Link.Source, req.Link.ID, req.Format, q.Res,
		int64(req.Start.Seconds()), int64(req.End.Seconds()))
}

// enqueue turns the finished dialog into a job: an instant answer from the
// file_id cache if there is one, otherwise a place in the queue.
func (b *Bot) enqueue(sess *session) {
	p := *sess.piece
	quality, _ := sess.info.Quality(sess.tier)
	req := source.Request{Link: sess.link, Info: sess.info, Format: sess.format, Tier: sess.tier}
	if sess.format == source.MP3 {
		quality = source.Quality{}
	}
	if !p.whole {
		req.Start, req.End = p.start, p.end
		req.Precise = sess.format == source.MP4 && p.length() <= b.cfg.PreciseCutMax
	}
	j := &job{
		userID: sess.userID, chatID: sess.chatID, msgID: sess.msgID,
		src: sess.src, req: req, info: sess.info, piece: p, quality: quality,
		key:         cacheKey(req, quality),
		ready:       make(chan struct{}),
		stopUpdates: make(chan struct{}),
		updatesDone: make(chan struct{}),
	}

	if fileID, ok := b.store.File(j.key); ok {
		if err := b.sendCached(j, fileID); err == nil {
			b.delete(j.chatID, j.msgID)
			b.stats.cacheHit()
			b.log.Info("served from cache", "user", j.userID, "video", j.info.ID)
			return
		}
		// Telegram no longer knows the file: forget it and download again.
		b.warnStore(b.store.DropFile(j.key))
	}

	isAdmin := slices.Contains(b.cfg.Admins, j.userID)
	if b.cfg.DailyQuota > 0 && !isAdmin && b.store.QuotaUsed(j.userID) >= b.cfg.DailyQuota {
		b.edit(j.chatID, j.msgID, textQuota(b.cfg), nil)
		return
	}

	task, err := b.queue.Submit(j.userID, func(ctx context.Context) { b.runJob(ctx, j) })
	if err != nil {
		text := textRestarting
		switch {
		case errors.Is(err, queue.ErrUserBusy):
			text = textUserBusy
		case errors.Is(err, queue.ErrFull):
			text = textQueueFull
		}
		b.edit(j.chatID, j.msgID, text, nil)
		return
	}
	j.task = task
	b.mu.Lock()
	b.jobs[task.ID] = j
	b.mu.Unlock()
	b.warnStore(b.store.QuotaAdd(j.userID, 1))

	text := textStatus(j, stageQueued, -1, task.Position())
	b.edit(j.chatID, j.msgID, text, cancelKeyboard(task.ID))
	go b.updateStatus(j, text)
	close(j.ready)
}

func cancelKeyboard(taskID string) models.ReplyMarkup {
	return keyboard(button(btnCancel, cbCancelJob, taskID, ""))
}

// updateStatus keeps the single status message current, editing it no more
// often than cfg.StatusEvery: Telegram asks for at most a message a second
// per chat.
func (b *Bot) updateStatus(j *job, last string) {
	defer close(j.updatesDone)
	tick := time.NewTicker(min(b.cfg.StatusEvery, time.Second))
	defer tick.Stop()

	edited := time.Now()
	for {
		select {
		case <-j.stopUpdates:
			return
		case <-tick.C:
		}
		if time.Since(edited) < b.cfg.StatusEvery {
			continue
		}
		st, percent := j.state()
		text := textStatus(j, st, percent, j.task.Position())
		if text == last {
			continue
		}
		var kb models.ReplyMarkup
		if st != stageSend {
			kb = cancelKeyboard(j.task.ID)
		}
		b.edit(j.chatID, j.msgID, text, kb)
		last, edited = text, time.Now()
	}
}

// runJob is called by the queue exactly once per job, possibly with an
// already cancelled context if the job never got to a worker.
func (b *Bot) runJob(ctx context.Context, j *job) {
	<-j.ready
	started := time.Now()
	err := b.process(ctx, j)

	close(j.stopUpdates)
	<-j.updatesDone
	// The job is forgotten only after its last word in the chat, so that
	// "no jobs" means the user has already seen how this one ended.
	defer func() {
		b.mu.Lock()
		delete(b.jobs, j.task.ID)
		b.mu.Unlock()
	}()

	took := time.Since(started).Round(time.Millisecond)
	log := b.log.With("user", j.userID, "video", j.info.ID, "format", j.req.Format, "took", took.String())
	cancelled := errors.Is(err, queue.ErrCancelled)
	switch {
	case err == nil:
		b.delete(j.chatID, j.msgID)
		b.stats.finished(outcomeDone, took)
		log.Info("job done")
		return
	case cancelled:
		b.stats.finished(outcomeCancelled, took)
		log.Info("job cancelled")
	default:
		b.stats.finished(outcomeFailed, took)
		log.Warn("job failed", "err", err)
	}

	// A job cancelled halfway has used the server, so it stays counted.
	if !cancelled || !j.worked {
		b.warnStore(b.store.QuotaAdd(j.userID, -1))
	}
	b.edit(j.chatID, j.msgID, textJobTitle(j)+"\n\n"+textJobError(err, b.cfg), nil)
}

// process downloads and sends the file. The temporary directory is removed
// whatever happens: success, error, cancellation or timeout.
func (b *Bot) process(ctx context.Context, j *job) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	dir, err := os.MkdirTemp(b.cfg.TmpDir, "job-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	j.mu.Lock()
	j.worked, j.stage, j.percent = true, stageDownload, -1
	j.mu.Unlock()

	req := j.req
	req.Dir = dir
	text := req.Format == formatText
	if text {
		req.Format = source.MP3
	}
	res, err := j.src.Download(ctx, req, func(p source.Progress) {
		if p.Stage == source.StageConvert && !text {
			j.set(stageConvert, p.Percent)
		} else {
			j.set(stageDownload, p.Percent)
		}
	})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	b.noteSourceResult(err)
	if err != nil {
		return err
	}
	if text {
		return b.transcribe(ctx, j, res.Path, dir)
	}
	if res.Size > b.cfg.MaxFileBytes {
		return &tooBigError{size: res.Size}
	}

	j.set(stageSend, -1)
	fileID, err := b.sendFile(ctx, j, res)
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errSend, err)
	}
	if fileID != "" {
		b.warnStore(b.store.PutFile(j.key, fileID))
	}
	return nil
}

// chatChunk leaves room for the "rest is in the file" note under Telegram's
// 4096 characters per message.
const (
	chatChunk    = 3900
	chatMessages = 5 // longer transcripts go to the chat only in part
)

// transcribe recognizes the speech in the downloaded audio and sends it as
// messages and as a .txt file with timecodes.
func (b *Bot) transcribe(ctx context.Context, j *job, audio, dir string) error {
	j.set(stageTranscribe, -1)
	res, err := b.cfg.Transcriber.Transcribe(ctx, audio, dir, func(p float64) { j.set(stageTranscribe, p) })
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errTranscribe, err)
	}
	if len(res.Segments) == 0 {
		return errNoSpeech
	}

	j.set(stageSend, -1)
	chunks := transcribe.Split(html.EscapeString(res.Plain()), chatChunk)
	for i, chunk := range chunks[:min(len(chunks), chatMessages)] {
		if i == chatMessages-1 && len(chunks) > chatMessages {
			chunk += "\n\n" + textRestInFile
		}
		b.send(j.chatID, chunk, nil)
	}

	path := filepath.Join(dir, "transcript.txt")
	if err := os.WriteFile(path, []byte(textTranscriptFile(j, res)), 0o600); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	upload := &models.InputFileUpload{Filename: fileName(j.info.Title, "txt"), Data: file}
	msg, err := b.api.SendDocument(ctx, b.documentParams(j, upload))
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errSend, err)
	}
	if msg != nil && msg.Document != nil {
		b.warnStore(b.store.PutFile(j.key, msg.Document.FileID))
	}
	return nil
}

func (b *Bot) documentParams(j *job, file models.InputFile) *tg.SendDocumentParams {
	return &tg.SendDocumentParams{
		ChatID:    j.chatID,
		Document:  file,
		Caption:   textCaption(j),
		ParseMode: models.ParseModeHTML,
	}
}

// sendFile uploads the result and returns its file_id for the cache.
func (b *Bot) sendFile(ctx context.Context, j *job, res *source.Result) (string, error) {
	file, err := os.Open(res.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var thumb models.InputFile
	if res.Thumb != "" {
		if t, err := os.Open(res.Thumb); err == nil {
			defer t.Close()
			thumb = &models.InputFileUpload{Filename: "thumb.jpg", Data: t}
		}
	}
	upload := &models.InputFileUpload{Filename: fileName(j.info.Title, string(j.req.Format)), Data: file}
	duration := int(res.Duration.Round(time.Second).Seconds())

	if j.req.Format == source.MP3 {
		msg, err := b.api.SendAudio(ctx, b.audioParams(j, upload, thumb, duration))
		if err != nil || msg.Audio == nil {
			return "", err
		}
		return msg.Audio.FileID, nil
	}
	params := b.videoParams(j, upload, thumb, duration)
	params.Width, params.Height = res.Width, res.Height
	msg, err := b.api.SendVideo(ctx, params)
	if err != nil || msg.Video == nil {
		// Telegram may store an odd file as a document: fine, but not cacheable as video.
		return "", err
	}
	return msg.Video.FileID, nil
}

// sendCached delivers a file Telegram already has.
func (b *Bot) sendCached(j *job, fileID string) error {
	ctx, cancel := apiContext()
	defer cancel()
	cached := &models.InputFileString{Data: fileID}
	switch j.req.Format {
	case formatText:
		_, err := b.api.SendDocument(ctx, b.documentParams(j, cached))
		return err
	case source.MP3:
		_, err := b.api.SendAudio(ctx, b.audioParams(j, cached, nil, 0))
		return err
	}
	_, err := b.api.SendVideo(ctx, b.videoParams(j, cached, nil, 0))
	return err
}

func (b *Bot) videoParams(j *job, file, thumb models.InputFile, duration int) *tg.SendVideoParams {
	return &tg.SendVideoParams{
		ChatID:            j.chatID,
		Video:             file,
		Thumbnail:         thumb,
		Duration:          duration,
		Caption:           textCaption(j),
		ParseMode:         models.ParseModeHTML,
		SupportsStreaming: true,
	}
}

func (b *Bot) audioParams(j *job, file, thumb models.InputFile, duration int) *tg.SendAudioParams {
	return &tg.SendAudioParams{
		ChatID:    j.chatID,
		Audio:     file,
		Thumbnail: thumb,
		Duration:  duration,
		Title:     truncate(j.info.Title, 200),
		Performer: truncate(j.info.Channel, 100),
		Caption:   textCaption(j),
		ParseMode: models.ParseModeHTML,
	}
}

// fileName makes a title safe to use as the name of the sent file.
func fileName(title, ext string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return -1
		}
		return r
	}, title)
	clean = strings.TrimSpace(truncate(strings.Join(strings.Fields(clean), " "), 80))
	if clean == "" {
		clean = "file"
	}
	return clean + "." + ext
}

// noteSourceResult feeds the failure-rate alert: errors caused by the video
// itself (private, deleted) say nothing about the health of the downloader.
func (b *Bot) noteSourceResult(err error) {
	failed := false
	if err != nil {
		var srcErr *source.Error
		if !errors.As(err, &srcErr) {
			return
		}
		if srcErr.Kind != source.KindUnknown && srcErr.Kind != source.KindBlocked {
			return
		}
		failed = true
	}
	if failedCount, total, alert := b.stats.sourceResult(failed); alert {
		b.log.Error("downloader failure rate is high", "failed", failedCount, "total", total)
		for _, admin := range b.cfg.Admins {
			b.send(admin, textAlert(failedCount, total, alertWindow), nil)
		}
	}
}

func (b *Bot) warnStore(err error) {
	if err != nil {
		b.log.Error("store write failed", "err", err)
	}
}

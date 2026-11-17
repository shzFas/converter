package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"converter/internal/queue"
	"converter/internal/source"
	"converter/internal/store"
)

const (
	userID = int64(1001)
	video  = "https://youtu.be/dQw4w9WgXcQ"
	mb     = int64(1) << 20
)

// fakeAPI is Telegram as the bot sees it: it keeps every message with its
// current text and keyboard.
type fakeAPI struct {
	mu       sync.Mutex
	lastID   int
	texts    map[int]string
	markups  map[int]models.ReplyMarkup
	order    []int // message IDs in the order they were sent
	deleted  map[int]bool
	answers  []string
	videos   []*tg.SendVideoParams
	audios   []*tg.SendAudioParams
	docs     []*tg.SendDocumentParams
	docData  []string // contents of the uploaded documents
	uploaded []int    // sizes of the uploaded files
	// rejectFileID makes sending by file_id fail, as for an expired file.
	rejectFileID bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{texts: map[int]string{}, markups: map[int]models.ReplyMarkup{}, deleted: map[int]bool{}}
}

func (f *fakeAPI) SendMessage(_ context.Context, p *tg.SendMessageParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID++
	f.texts[f.lastID], f.markups[f.lastID] = p.Text, p.ReplyMarkup
	f.order = append(f.order, f.lastID)
	return &models.Message{ID: f.lastID}, nil
}

func (f *fakeAPI) EditMessageText(_ context.Context, p *tg.EditMessageTextParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts[p.MessageID], f.markups[p.MessageID] = p.Text, p.ReplyMarkup
	return &models.Message{ID: p.MessageID}, nil
}

func (f *fakeAPI) DeleteMessage(_ context.Context, p *tg.DeleteMessageParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted[p.MessageID] = true
	return true, nil
}

func (f *fakeAPI) AnswerCallbackQuery(_ context.Context, p *tg.AnswerCallbackQueryParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, p.Text)
	return true, nil
}

func (f *fakeAPI) upload(file models.InputFile) error {
	switch v := file.(type) {
	case *models.InputFileUpload:
		data, err := io.ReadAll(v.Data)
		f.uploaded = append(f.uploaded, len(data))
		return err
	case *models.InputFileString:
		if f.rejectFileID {
			return fmt.Errorf("%w, wrong file identifier", tg.ErrorBadRequest)
		}
	}
	return nil
}

func (f *fakeAPI) SendVideo(_ context.Context, p *tg.SendVideoParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.upload(p.Video); err != nil {
		return nil, err
	}
	f.videos = append(f.videos, p)
	return &models.Message{Video: &models.Video{FileID: fmt.Sprintf("video-%d", len(f.videos))}}, nil
}

func (f *fakeAPI) SendAudio(_ context.Context, p *tg.SendAudioParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.upload(p.Audio); err != nil {
		return nil, err
	}
	f.audios = append(f.audios, p)
	return &models.Message{Audio: &models.Audio{FileID: fmt.Sprintf("audio-%d", len(f.audios))}}, nil
}

func (f *fakeAPI) SendDocument(_ context.Context, p *tg.SendDocumentParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := p.Document.(*models.InputFileUpload); ok {
		data, err := io.ReadAll(v.Data)
		if err != nil {
			return nil, err
		}
		f.docData = append(f.docData, string(data))
		v.Data = strings.NewReader(string(data))
	}
	if err := f.upload(p.Document); err != nil {
		return nil, err
	}
	f.docs = append(f.docs, p)
	return &models.Message{Document: &models.Document{FileID: fmt.Sprintf("doc-%d", len(f.docs))}}, nil
}

// last returns the newest message that has not been deleted.
func (f *fakeAPI) last() (id int, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.order) - 1; i >= 0; i-- {
		if id := f.order[i]; !f.deleted[id] {
			return id, f.texts[id]
		}
	}
	return 0, ""
}

func (f *fakeAPI) buttons(msgID int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	labels := map[string]string{}
	if kb, ok := f.markups[msgID].(*models.InlineKeyboardMarkup); ok && kb != nil {
		for _, row := range kb.InlineKeyboard {
			for _, btn := range row {
				labels[btn.Text] = btn.CallbackData
			}
		}
	}
	return labels
}

func (f *fakeAPI) text(msgID int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.texts[msgID]
}

func (f *fakeAPI) isDeleted(msgID int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[msgID]
}

func (f *fakeAPI) video(i int) *tg.SendVideoParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.videos[i]
}

func (f *fakeAPI) audio(i int) *tg.SendAudioParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audios[i]
}

func (f *fakeAPI) lastAnswer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return ""
	}
	return f.answers[len(f.answers)-1]
}

func (f *fakeAPI) sent() (videos, audios int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.videos), len(f.audios)
}

// fakeSource stands in for yt-dlp: a ten-minute video in three qualities,
// of which 1080p is too big to send whole.
type fakeSource struct {
	mu       sync.Mutex
	info     source.Info
	probeErr error
	probes   int
	requests []source.Request
	download func(ctx context.Context, req source.Request, progress func(source.Progress)) (*source.Result, error)
}

func newFakeSource() *fakeSource {
	return &fakeSource{info: source.Info{
		ID: "dQw4w9WgXcQ", Title: "Test <video>", Channel: "Channel & Co", Duration: 10 * time.Minute,
		Qualities: []source.Quality{
			{Tier: 360, Res: 360, Label: "360p", VideoBytes: 10 * mb},
			{Tier: 720, Res: 720, Label: "720p", VideoBytes: 30 * mb},
			{Tier: 1080, Res: 1080, Label: "1080p", VideoBytes: 80 * mb},
		},
		AudioBytes: 5 * mb,
	}}
}

func (s *fakeSource) Name() string { return "youtube" }

func (s *fakeSource) Match(raw string) (source.Link, error) {
	return source.NewYouTube(source.YouTubeConfig{}).Match(raw)
}

func (s *fakeSource) Probe(context.Context, source.Link) (*source.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes++
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	info := s.info
	return &info, nil
}

func (s *fakeSource) Download(ctx context.Context, req source.Request, progress func(source.Progress)) (*source.Result, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	download := s.download
	s.mu.Unlock()
	if download != nil {
		return download(ctx, req, progress)
	}
	progress(source.Progress{Stage: source.StageDownload, Percent: 42})
	path := filepath.Join(req.Dir, "media."+string(req.Format))
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		return nil, err
	}
	return &source.Result{Path: path, Size: 5, Duration: 15 * time.Second, Width: 1280, Height: 720}, nil
}

func (s *fakeSource) request(t *testing.T, i int) source.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.requests) {
		t.Fatalf("download #%d was not requested (%d so far)", i+1, len(s.requests))
	}
	return s.requests[i]
}

func (s *fakeSource) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

func (s *fakeSource) downloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// harness wires a Bot to the fakes.
type harness struct {
	t     *testing.T
	bot   *Bot
	api   *fakeAPI
	src   *fakeSource
	store *store.Store
	tmp   string
}

func newHarness(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DailyQuota:    20,
		MaxFileBytes:  48 * mb,
		MaxDuration:   60 * time.Minute,
		PreciseCutMax: 10 * time.Minute,
		DefaultTier:   720,
		JobTimeout:    10 * time.Minute,
		InputTimeout:  time.Minute,
		StatusEvery:   5 * time.Millisecond,
		TmpDir:        filepath.Join(dir, "tmp"),
	}
	if tune != nil {
		tune(&cfg)
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	q := queue.New(queue.Config{Workers: 1, Size: 5, PerUser: 1})
	h := &harness{t: t, api: newFakeAPI(), src: newFakeSource(), store: st, tmp: cfg.TmpDir}
	h.bot = New(cfg, []source.Source{h.src}, q, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.bot.Attach(h.api)
	t.Cleanup(func() { h.bot.Shutdown(context.Background()) })
	return h
}

func (h *harness) say(text string) { h.sayAs(userID, text) }

func (h *harness) sayAs(user int64, text string) {
	h.bot.dispatch(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: user},
		Chat: models.Chat{ID: user, Type: models.ChatTypePrivate},
		Text: text,
	}})
}

// press pushes the button of the last message whose label contains the text.
func (h *harness) press(label string) {
	h.t.Helper()
	msgID, text := h.api.last()
	for btn, data := range h.api.buttons(msgID) {
		if strings.Contains(btn, label) {
			h.callback(msgID, data)
			return
		}
	}
	h.t.Fatalf("no button %q under %q, have %v", label, text, h.api.buttons(msgID))
}

func (h *harness) callback(msgID int, data string) {
	h.bot.dispatch(context.Background(), &models.Update{CallbackQuery: &models.CallbackQuery{
		ID:      "cb",
		From:    models.User{ID: userID},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: msgID}},
		Data:    data,
	}})
}

func (h *harness) expectLast(want string) {
	h.t.Helper()
	if _, text := h.api.last(); !strings.Contains(text, want) {
		h.t.Fatalf("last message = %q, want it to contain %q", text, want)
	}
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			_, text := h.api.last()
			h.t.Fatalf("timed out waiting for %s; last message: %q", what, text)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) waitVideos(n int) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("%d sent videos", n), func() bool { v, _ := h.api.sent(); return v == n })
}

func (h *harness) waitIdle() {
	h.t.Helper()
	h.eventually("jobs to finish", func() bool { return len(h.bot.userJobs(userID)) == 0 })
}

func (h *harness) tmpIsEmpty() bool {
	entries, err := os.ReadDir(h.tmp)
	return err == nil && len(entries) == 0
}

func TestFullDialog(t *testing.T) {
	h := newHarness(t, nil)

	h.say(video)
	cardID, card := h.api.last()
	// The title goes into HTML, so it must be escaped.
	for _, want := range []string{"Test &lt;video&gt;", "Channel &amp; Co", "10:00", "Выберите формат"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want %q in it", card, want)
		}
	}

	h.press("MP4")
	h.expectLast("Выберите качество")
	buttons := h.api.buttons(cardID)
	for label := range buttons {
		// 30+5 MB fits the 48 MB limit, 80+5 MB only as a fragment.
		if strings.HasPrefix(label, "1080p") != strings.Contains(label, btnFragmentOf) {
			t.Errorf("unexpected quality button %q", label)
		}
	}
	if len(buttons) != 3 {
		t.Fatalf("quality buttons = %v, want 3", buttons)
	}

	h.press("720p")
	h.expectLast("Что скачать?")
	h.press("Весь ролик")
	h.waitVideos(1)
	h.waitIdle()

	req := h.src.request(t, 0)
	if req.Format != source.MP4 || req.Tier != 720 || !req.Whole() {
		t.Errorf("request = %+v, want whole MP4 720", req)
	}
	sent := h.api.video(0)
	if sent.Width != 1280 || sent.Height != 720 || sent.Duration != 15 || !sent.SupportsStreaming {
		t.Errorf("video params = %+v", sent)
	}
	if !h.api.isDeleted(cardID) {
		t.Error("status message was not deleted after delivery")
	}
	if !h.tmpIsEmpty() {
		t.Error("temporary files were left behind")
	}
	if used := h.store.QuotaUsed(userID); used != 1 {
		t.Errorf("quota used = %d, want 1", used)
	}
}

func TestLinkWithTimecodesSkipsRangeStep(t *testing.T) {
	h := newHarness(t, nil)

	h.say(video + " 1:30-2:45")
	h.expectLast("1:30–2:45 (1:15)")
	h.press("MP3")
	h.eventually("audio", func() bool { _, a := h.api.sent(); return a == 1 })
	h.waitIdle()

	req := h.src.request(t, 0)
	if req.Format != source.MP3 || req.Start != 90*time.Second || req.End != 165*time.Second {
		t.Errorf("request = %+v, want MP3 90s-165s", req)
	}
	audio := h.api.audio(0)
	if audio.Title != "Test <video>" || audio.Performer != "Channel & Co" {
		t.Errorf("audio tags = %q / %q", audio.Title, audio.Performer)
	}
}

func TestTypedTimecodes(t *testing.T) {
	h := newHarness(t, nil)

	h.say(video)
	h.press("MP4")
	h.press("1080p") // too big as a whole
	_, text := h.api.last()
	if !strings.Contains(text, "Целиком не получится") {
		t.Fatalf("range step = %q", text)
	}
	cardID, _ := h.api.last()
	for label := range h.api.buttons(cardID) {
		if strings.Contains(label, "Весь ролик") {
			t.Fatal("the whole video is offered although it is over the limit")
		}
	}

	h.press("Вырезать фрагмент")
	h.expectLast("Напишите таймкоды")

	h.say("полторы минуты")
	h.expectLast("Не понял таймкоды")
	h.say("2:45-1:30")
	h.expectLast("Начало должно быть раньше конца")
	h.say("11:00-12:00")
	h.expectLast("длительность 10:00")
	h.say("0:00-9:00") // 9 of 10 minutes of 85 MB is still too much
	h.expectLast("пришлите таймкоды покороче")
	if h.src.downloads() != 0 {
		t.Fatal("a download started on invalid timecodes")
	}

	h.say("1:30 – 2:45")
	h.waitVideos(1)
	h.waitIdle()
	req := h.src.request(t, 0)
	if req.Tier != 1080 || req.Start != 90*time.Second || req.End != 165*time.Second || !req.Precise {
		t.Errorf("request = %+v, want precise 1080p 90s-165s", req)
	}
	if caption := h.api.video(0).Caption; !strings.Contains(caption, "1:30–2:45") {
		t.Errorf("caption = %q", caption)
	}
}

func TestLongFragmentIsCutFast(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PreciseCutMax = time.Minute })

	h.say(video + " 1:00-3:00")
	h.press("MP4")
	h.press("360p")
	h.waitVideos(1)
	if req := h.src.request(t, 0); req.Precise {
		t.Error("a fragment over the precise limit was cut precisely")
	}
	if caption := h.api.video(0).Caption; !strings.Contains(caption, "Быстрый рез") {
		t.Errorf("caption = %q, want the fast cut warning", caption)
	}
}

func TestEndBeyondVideoIsClamped(t *testing.T) {
	h := newHarness(t, nil)
	h.say(video + " 9:30-15:00")
	h.expectLast("9:30–10:00 (0:30)")
}

func TestStartOffsetFromLink(t *testing.T) {
	h := newHarness(t, nil)

	h.say(video + "?t=90")
	h.press("MP3")
	h.press("С 1:30 до конца")
	h.eventually("audio", func() bool { _, a := h.api.sent(); return a == 1 })
	if req := h.src.request(t, 0); req.Start != 90*time.Second || req.End != 10*time.Minute {
		t.Errorf("request = %v-%v, want 1m30s-10m", req.Start, req.End)
	}
}

func TestCacheServesRepeat(t *testing.T) {
	h := newHarness(t, nil)
	order := func() {
		h.say("/mp4 " + video + " 0:10-0:20")
	}

	order()
	h.waitVideos(1)
	h.waitIdle()
	order()
	h.waitVideos(2)

	if n := h.src.downloads(); n != 1 {
		t.Fatalf("downloads = %d, want 1: the repeat must come from the cache", n)
	}
	cached, ok := h.api.video(1).Video.(*models.InputFileString)
	if !ok || cached.Data != "video-1" {
		t.Fatalf("repeat was sent as %#v, want file_id video-1", h.api.video(1).Video)
	}
	if used := h.store.QuotaUsed(userID); used != 1 {
		t.Errorf("quota used = %d: a cache hit must be free", used)
	}

	// A file_id Telegram rejects is forgotten and the video is downloaded again.
	h.api.mu.Lock()
	h.api.rejectFileID = true
	h.api.mu.Unlock()
	order()
	h.eventually("second download", func() bool { return h.src.downloads() == 2 })
}

func TestFastCommands(t *testing.T) {
	h := newHarness(t, nil)

	h.say("/mp4 " + video)
	h.waitVideos(1)
	h.waitIdle()
	if req := h.src.request(t, 0); req.Tier != 720 || !req.Whole() {
		t.Errorf("/mp4 request = %+v, want whole 720p", req)
	}

	h.say("/mp3")
	h.expectLast("/mp3 ссылка")
	h.say("/mp3 https://vk.com/video1")
	h.expectLast("только YouTube")
}

func TestFastCommandFallsToLowerQuality(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxFileBytes = 20 * mb })

	h.say("/mp4 " + video) // 720p is 35 MB, 360p is 15 MB
	h.waitVideos(1)
	if req := h.src.request(t, 0); req.Tier != 360 {
		t.Errorf("tier = %d, want 360", req.Tier)
	}
}

func TestWholeVideoOverLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxFileBytes = 10 * mb })

	// Even 360p is 15 MB: /mp4 has to ask which quality to cut a fragment from.
	h.say("/mp4 " + video)
	h.expectLast("Выберите качество")
	h.press("360p")
	h.expectLast("Целиком не получится: файл выйдет около 15 МБ при лимите 10 МБ")
	h.press("Вырезать фрагмент")
	h.say("0:00-1:00")
	h.waitVideos(1)
	h.waitIdle()

	// Ten minutes of MP3 are 14 MB: the same question instead of a refusal.
	h.say("/mp3 " + video)
	h.expectLast("Целиком не получится")
	if h.src.downloads() != 1 {
		t.Fatal("a download over the limit was started")
	}
}

func TestFragmentFitsOnlyAsMP3(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxFileBytes = 10 * mb })
	h.src.info.Qualities = h.src.info.Qualities[:1]
	h.src.info.Qualities[0].VideoBytes = 40 * mb // 4.5 MB a minute with audio

	h.say(video + " 0:00-5:00")
	h.press("MP4")
	h.expectLast("Не получится: файл выйдет около 23 МБ при лимите 10 МБ")
	h.press("MP3") // 7 MB
	h.eventually("audio", func() bool { _, a := h.api.sent(); return a == 1 })
	if videos, _ := h.api.sent(); videos != 0 {
		t.Fatal("a video over the limit was sent")
	}
}

func TestUnsupportedInput(t *testing.T) {
	h := newHarness(t, nil)
	tests := map[string]string{
		"привет":                                  textNoLink,
		"https://vimeo.com/123":                   textNotSupported,
		"tiktok.com/@user/video/1":                textNotSupported,
		"https://www.youtube.com/playlist?list=1": textNotVideo,
		"https://www.youtube.com/@channel":        textNotVideo,
		"/unknown":                                textUnknownCmd,
		"/stats":                                  textUnknownCmd, // not an admin
	}
	for input, want := range tests {
		h.say(input)
		if _, got := h.api.last(); got != want {
			t.Errorf("%q → %q, want %q", input, got, want)
		}
	}
	if n := h.src.probeCount(); n != 0 {
		t.Errorf("probes = %d, want none for unsupported input", n)
	}
}

func TestSourceErrors(t *testing.T) {
	tests := map[source.Kind]string{
		source.KindPrivate:       "приватное",
		source.KindUnavailable:   "недоступно",
		source.KindAgeRestricted: "возрастное",
		source.KindBlocked:       "ограничивает сервер",
		source.KindUnknown:       "Не удалось скачать",
	}
	for kind, want := range tests {
		h := newHarness(t, nil)
		h.src.probeErr = &source.Error{Kind: kind}
		h.say(video)
		h.expectLast(want)
	}

	h := newHarness(t, nil)
	h.src.info.Live = true
	h.say(video)
	h.expectLast("Прямые эфиры")
}

func TestCancelRunningJob(t *testing.T) {
	h := newHarness(t, nil)
	started := make(chan struct{})
	h.src.download = func(ctx context.Context, req source.Request, _ func(source.Progress)) (*source.Result, error) {
		if err := os.WriteFile(filepath.Join(req.Dir, "partial"), []byte("x"), 0o600); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}

	h.say("/mp3 " + video)
	<-started
	statusID, _ := h.api.last()

	// A second request while the first one runs is refused.
	h.say("/mp3 " + video + " 0:10-0:20")
	h.expectLast("уже есть активная задача")

	h.say("/cancel")
	h.waitIdle()
	if text := h.api.text(statusID); !strings.Contains(text, "Отменено") {
		t.Fatalf("status = %q, want it cancelled", text)
	}
	if !h.tmpIsEmpty() {
		t.Error("temporary files were left after cancel")
	}

	h.say("/cancel")
	h.expectLast(textNothingToStop)
}

func TestCancelButtonAndQueuePosition(t *testing.T) {
	h := newHarness(t, nil)
	release := make(chan struct{})
	h.src.download = func(ctx context.Context, req source.Request, _ func(source.Progress)) (*source.Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
		path := filepath.Join(req.Dir, "media.mp3")
		return &source.Result{Path: path, Size: 1}, os.WriteFile(path, []byte("x"), 0o600)
	}

	// Another user occupies the only worker.
	h.sayAs(2002, "/mp3 "+video)
	h.eventually("first job to start", func() bool { return h.src.downloads() == 1 })

	h.say("/mp3 " + video + " 0:10-0:20")
	statusID, _ := h.api.last()
	h.expectLast("В очереди, вы 1-й")

	h.press(btnCancel)
	h.waitIdle()
	if text := h.api.text(statusID); !strings.Contains(text, "Отменено") {
		t.Fatalf("status = %q, want it cancelled", text)
	}
	if used := h.store.QuotaUsed(userID); used != 0 {
		t.Errorf("quota used = %d: a job cancelled in the queue must be refunded", used)
	}
	close(release)
}

func TestDailyQuota(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DailyQuota = 1 })

	h.say("/mp3 " + video)
	h.eventually("audio", func() bool { _, a := h.api.sent(); return a == 1 })
	h.waitIdle()
	h.say("/mp3 " + video + " 0:10-0:20")
	h.expectLast("лимит исчерпан")
}

func TestFailedJobIsRefunded(t *testing.T) {
	h := newHarness(t, nil)
	h.src.download = func(context.Context, source.Request, func(source.Progress)) (*source.Result, error) {
		return nil, &source.Error{Kind: source.KindBlocked}
	}
	h.say("/mp3 " + video)
	h.waitIdle()
	h.expectLast("ограничивает сервер")
	if used := h.store.QuotaUsed(userID); used != 0 {
		t.Errorf("quota used = %d, want a refund after a failure", used)
	}
}

func TestOversizedResult(t *testing.T) {
	h := newHarness(t, nil)
	h.src.download = func(_ context.Context, req source.Request, _ func(source.Progress)) (*source.Result, error) {
		return &source.Result{Path: filepath.Join(req.Dir, "media.mp3"), Size: 49 * mb}, nil
	}
	h.say("/mp3 " + video)
	h.waitIdle()
	h.expectLast("больше лимита")
	if _, audios := h.api.sent(); audios != 0 {
		t.Error("an oversized file was sent")
	}
}

func TestInputTimeout(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InputTimeout = 20 * time.Millisecond })

	h.say(video)
	h.press("MP3")
	h.press("Вырезать фрагмент")
	h.eventually("the dialog to expire", func() bool {
		_, text := h.api.last()
		return text == textInputExpired
	})
	h.say("1:30-2:45")
	h.expectLast(textNoLink)
}

func TestCancelLeavesDialog(t *testing.T) {
	h := newHarness(t, nil)
	h.say(video)
	h.press("MP3")
	h.press("Вырезать фрагмент")
	h.say("/cancel")
	h.expectLast(textDialogStopped)
	h.say("1:30-2:45")
	h.expectLast(textNoLink)
}

func TestStaleButton(t *testing.T) {
	h := newHarness(t, nil)
	h.say(video)
	oldCard, _ := h.api.last()
	oldButtons := h.api.buttons(oldCard)

	h.say(video) // a new link replaces the dialog
	for _, data := range oldButtons {
		h.callback(oldCard, data)
		if got := h.api.lastAnswer(); got != textStale {
			t.Fatalf("answer to a stale button = %q, want %q", got, textStale)
		}
	}
	if h.src.downloads() != 0 {
		t.Fatal("a stale button started a download")
	}
}

func TestAccessLists(t *testing.T) {
	const admin, friend, stranger = int64(1), int64(2), int64(3)
	h := newHarness(t, func(c *Config) { c.Admins = []int64{admin} })

	// Open to everyone until the allow-list is used.
	h.sayAs(stranger, "/start")
	h.expectLast("Пришлите ссылку")

	h.sayAs(admin, fmt.Sprintf("/allow %d", friend))
	h.expectLast("добавлен в белый список")
	h.sayAs(stranger, "/start")
	h.expectLast(fmt.Sprintf("Ваш ID: <code>%d</code>", stranger))
	h.sayAs(friend, "/start")
	h.expectLast("Пришлите ссылку")

	h.sayAs(admin, fmt.Sprintf("/ban %d", friend))
	h.sayAs(friend, "/start")
	h.expectLast(textBanned)

	h.sayAs(admin, "/stats")
	h.expectLast("Статистика")
	h.sayAs(admin, "/allow abc")
	h.expectLast("/allow ID")

	h.sayAs(admin, "/blockvideo "+video)
	h.sayAs(admin, video)
	h.expectLast(textVideoBlocked)
}

func TestGroupsAreIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.bot.dispatch(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: userID},
		Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		Text: video,
	}})
	if id, _ := h.api.last(); id != 0 {
		t.Fatal("the bot answered in a group")
	}
}

func TestShutdownTellsQueuedUsers(t *testing.T) {
	h := newHarness(t, nil)
	h.src.download = func(ctx context.Context, _ source.Request, _ func(source.Progress)) (*source.Result, error) {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	h.sayAs(2002, "/mp3 "+video)
	h.eventually("first job to start", func() bool { return h.src.downloads() == 1 })
	h.say("/mp3 " + video + " 0:10-0:20")
	queuedID, _ := h.api.last()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	h.bot.Shutdown(ctx)

	if text := h.api.text(queuedID); !strings.Contains(text, "перезапускается") {
		t.Fatalf("queued user was told %q", text)
	}
	if !h.tmpIsEmpty() {
		t.Error("temporary files were left after shutdown")
	}
}

func TestAlertOnFailureRate(t *testing.T) {
	const admin = int64(1)
	h := newHarness(t, func(c *Config) { c.Admins = []int64{admin} })
	h.src.probeErr = &source.Error{Kind: source.KindUnknown}

	alerts := func() int {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		n := 0
		for _, text := range h.api.texts {
			if strings.HasPrefix(text, "⚠️") {
				n++
			}
		}
		return n
	}
	for i := range alertMinSamples + 3 {
		h.say(fmt.Sprintf("https://youtu.be/AAAAAAAAA%02d", i))
	}
	if n := alerts(); n != 1 {
		t.Fatalf("alerts = %d, want exactly one per window", n)
	}
}

func TestUpdatesOfOneUserKeepOrder(t *testing.T) {
	h := newHarness(t, nil)
	text := func(s string) *models.Update {
		return &models.Update{Message: &models.Message{
			From: &models.User{ID: userID},
			Chat: models.Chat{ID: userID, Type: models.ChatTypePrivate},
			Text: s,
		}}
	}
	// Handle returns at once; the answers must still come in the order asked.
	inputs := []string{"/start", "/help", "привет", "https://vimeo.com/1", "/unknown", "/start", "/help"}
	for _, in := range inputs {
		h.bot.Handle(context.Background(), text(in))
	}
	h.eventually("all answers", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return len(h.api.order) == len(inputs)
	})
	want := []string{"Пришлите ссылку на видео с YouTube — верну", "Как пользоваться", textNoLink, textNotSupported, textUnknownCmd, "верну MP4", "Как пользоваться"}
	for i, id := range h.api.order {
		if got := h.api.text(id); !strings.Contains(got, want[i]) {
			t.Errorf("answer %d = %q, want %q", i+1, got, want[i])
		}
	}
}

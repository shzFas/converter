// Package bot is the Telegram side: the dialog, keyboards, limits and the
// life of a job from the queue to the file in the chat.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"converter/internal/queue"
	"converter/internal/source"
	"converter/internal/store"
	"converter/internal/transcribe"
)

// API is the part of the Telegram client the bot uses; *tg.Bot implements it.
type API interface {
	SendMessage(ctx context.Context, params *tg.SendMessageParams) (*models.Message, error)
	EditMessageText(ctx context.Context, params *tg.EditMessageTextParams) (*models.Message, error)
	DeleteMessage(ctx context.Context, params *tg.DeleteMessageParams) (bool, error)
	AnswerCallbackQuery(ctx context.Context, params *tg.AnswerCallbackQueryParams) (bool, error)
	SendVideo(ctx context.Context, params *tg.SendVideoParams) (*models.Message, error)
	SendAudio(ctx context.Context, params *tg.SendAudioParams) (*models.Message, error)
	SendDocument(ctx context.Context, params *tg.SendDocumentParams) (*models.Message, error)
}

// Config holds the limits and settings of the dialog.
type Config struct {
	DailyQuota    int           // tasks per user a day, 0 for no limit
	MaxFileBytes  int64         // largest file Telegram accepts from the bot
	MaxDuration   time.Duration // longest video or fragment
	PreciseCutMax time.Duration // MP4 fragments up to this length are cut frame-accurately
	DefaultTier   int           // video quality for /mp4
	JobTimeout    time.Duration // only shown to the user; the queue enforces it
	InputTimeout  time.Duration // how long to wait for typed timecodes
	StatusEvery   time.Duration // minimal interval between edits of the status message
	TmpDir        string        // parent of the per-job temporary directories
	Admins        []int64
	Allowed       []int64 // allow-list from the environment, see Bot.access
	// Transcriber turns the audio into text; nil hides the text option.
	Transcriber transcribe.Transcriber
}

// Bot handles updates. Create it with New, then Attach the Telegram client.
type Bot struct {
	cfg     Config
	api     API
	sources []source.Source
	queue   *queue.Queue
	store   *store.Store
	log     *slog.Logger
	stats   *stats

	mu       sync.Mutex
	sessions map[int64]*session
	jobs     map[string]*job
	inbox    map[int64][]*models.Update // updates waiting per user, see Handle
	users    map[int64]*sync.Mutex
	probes   map[string]probeEntry
	probeSem chan struct{}
}

func New(cfg Config, sources []source.Source, q *queue.Queue, st *store.Store, log *slog.Logger) *Bot {
	return &Bot{
		cfg:      cfg,
		sources:  sources,
		queue:    q,
		store:    st,
		log:      log,
		stats:    newStats(),
		sessions: make(map[int64]*session),
		jobs:     make(map[string]*job),
		inbox:    make(map[int64][]*models.Update),
		users:    make(map[int64]*sync.Mutex),
		probes:   make(map[string]probeEntry),
		probeSem: make(chan struct{}, 4),
	}
}

// Attach sets the Telegram client. It must be called before the first update.
func (b *Bot) Attach(api API) { b.api = api }

// Handler adapts Handle to the go-telegram/bot handler signature.
func (b *Bot) Handler(ctx context.Context, _ *tg.Bot, update *models.Update) {
	b.Handle(ctx, update)
}

// inboxLimit bounds the updates of one user waiting to be handled; a flood
// beyond it is dropped.
const inboxLimit = 20

// Handle accepts an update and returns at once. Updates of one user are
// handled strictly in the order Handle received them — "link, then /cancel"
// must never run backwards — while different users do not wait for each
// other. The caller must therefore deliver updates sequentially.
func (b *Bot) Handle(ctx context.Context, update *models.Update) {
	userID, ok := updateUser(update)
	if !ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	waiting, draining := b.inbox[userID]
	if len(waiting) >= inboxLimit {
		return
	}
	b.inbox[userID] = append(waiting, update)
	if !draining {
		go b.drain(ctx, userID)
	}
}

// drain handles the user's updates one by one until none are left.
func (b *Bot) drain(ctx context.Context, userID int64) {
	for {
		b.mu.Lock()
		waiting := b.inbox[userID]
		if len(waiting) == 0 {
			delete(b.inbox, userID)
			b.mu.Unlock()
			return
		}
		update := waiting[0]
		b.inbox[userID] = waiting[1:]
		b.mu.Unlock()
		b.dispatch(ctx, update)
	}
}

// updateUser tells whose update it is; updates outside private chats
// (groups, channels) are out of scope and have no user here.
func updateUser(update *models.Update) (int64, bool) {
	switch {
	case update.Message != nil:
		if msg := update.Message; msg.From != nil && msg.Chat.Type == models.ChatTypePrivate {
			return msg.From.ID, true
		}
	case update.CallbackQuery != nil:
		return update.CallbackQuery.From.ID, true
	}
	return 0, false
}

// dispatch handles one update to the end. The user's lock is also taken by
// the input timeout timer, so the dialog state never changes under a
// handler's feet.
func (b *Bot) dispatch(ctx context.Context, update *models.Update) {
	userID, ok := updateUser(update)
	if !ok {
		return
	}
	unlock := b.lockUser(userID)
	defer unlock()
	if update.Message != nil {
		b.onMessage(ctx, update.Message)
	} else {
		b.onCallback(ctx, update.CallbackQuery)
	}
}

func (b *Bot) lockUser(userID int64) (unlock func()) {
	b.mu.Lock()
	m := b.users[userID]
	if m == nil {
		m = new(sync.Mutex)
		b.users[userID] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

type accessLevel int

const (
	accessUser accessLevel = iota
	accessAdmin
	accessClosed // the allow-list is in use and the user is not on it
	accessBanned
)

// access decides what the user may do. The bot is open to everyone until an
// allow-list appears, either in ALLOWED_USERS or through /allow.
func (b *Bot) access(userID int64) accessLevel {
	if slices.Contains(b.cfg.Admins, userID) {
		return accessAdmin
	}
	if b.store.Denied(userID) {
		return accessBanned
	}
	allowed, storeListInUse := b.store.Allowed(userID)
	if !storeListInUse && len(b.cfg.Allowed) == 0 {
		return accessUser
	}
	if allowed || slices.Contains(b.cfg.Allowed, userID) {
		return accessUser
	}
	return accessClosed
}

func (b *Bot) onMessage(ctx context.Context, msg *models.Message) {
	userID, chatID := msg.From.ID, msg.Chat.ID

	level := b.access(userID)
	switch level {
	case accessBanned:
		b.send(chatID, textBanned, nil)
		return
	case accessClosed:
		b.send(chatID, textClosed(userID), nil)
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		b.send(chatID, textNoLink, nil)
		return
	}
	if strings.HasPrefix(text, "/") {
		b.onCommand(ctx, chatID, userID, level, text)
		return
	}

	// A message while timecodes are awaited is the timecodes, unless it has a link.
	sess := b.session(userID)
	link, src, rest, err := b.findLink(text)
	if errors.Is(err, source.ErrNoMatch) && !looksLikeLink(text) {
		if sess != nil && sess.step == stepTimecode {
			b.onTimecodes(sess, text)
			return
		}
		b.send(chatID, textNoLink, nil)
		return
	}
	if err != nil {
		b.send(chatID, linkErrorText(err), nil)
		return
	}
	b.startDialog(ctx, chatID, userID, src, link, rest, "")
}

func (b *Bot) onCommand(ctx context.Context, chatID, userID int64, level accessLevel, text string) {
	name, arg, _ := strings.Cut(text, " ")
	name, _, _ = strings.Cut(strings.ToLower(name[1:]), "@")
	arg = strings.TrimSpace(arg)

	switch name {
	case "start":
		b.send(chatID, textStart(), nil)
	case "help":
		b.send(chatID, textHelp(b.cfg), nil)
	case "cancel":
		b.onCancelCommand(chatID, userID)
	case "mp3", "mp4":
		link, src, rest, err := b.findLink(arg)
		switch {
		case arg == "" || (errors.Is(err, source.ErrNoMatch) && !looksLikeLink(arg)):
			b.send(chatID, strings.ReplaceAll(textFastUsage, "%s", name), nil)
		case err != nil:
			b.send(chatID, linkErrorText(err), nil)
		default:
			b.startDialog(ctx, chatID, userID, src, link, rest, source.Format(name))
		}
	case "text":
		link, src, rest, err := b.findLink(arg)
		switch {
		case b.cfg.Transcriber == nil:
			b.send(chatID, textNoTranscriber, nil)
		case arg == "" || (errors.Is(err, source.ErrNoMatch) && !looksLikeLink(arg)):
			b.send(chatID, strings.ReplaceAll(textFastUsage, "%s", name), nil)
		case err != nil:
			b.send(chatID, linkErrorText(err), nil)
		default:
			b.startDialog(ctx, chatID, userID, src, link, rest, formatText)
		}
	default:
		if level == accessAdmin && b.onAdminCommand(chatID, name, arg) {
			return
		}
		b.send(chatID, textUnknownCmd, nil)
	}
}

func linkErrorText(err error) string {
	if errors.Is(err, source.ErrNotVideo) {
		return textNotVideo
	}
	return textNotSupported
}

// findLink looks for the first word a source recognizes and returns what
// is left of the message, which may be timecodes.
func (b *Bot) findLink(text string) (source.Link, source.Source, string, error) {
	words := strings.Fields(text)
	result := source.ErrNoMatch
	for i, word := range words {
		for _, src := range b.sources {
			link, err := src.Match(word)
			if err == nil {
				rest := strings.Join(append(slices.Clone(words[:i]), words[i+1:]...), " ")
				return link, src, rest, nil
			}
			if errors.Is(err, source.ErrNotVideo) {
				result = source.ErrNotVideo
			}
		}
	}
	return source.Link{}, nil, "", result
}

// looksLikeLink tells a foreign link ("only YouTube is supported") from
// plain text ("send me a link").
func looksLikeLink(text string) bool {
	return slices.ContainsFunc(strings.Fields(text), linkLike.MatchString)
}

var linkLike = regexp.MustCompile(`(?i)^(https?://|www\.|[a-z0-9-]+(\.[a-z0-9-]+)+/)`)

// onCancelCommand stops the user's job, or leaves the dialog if there is none.
func (b *Bot) onCancelCommand(chatID, userID int64) {
	cancelled, sending := 0, 0
	for _, j := range b.userJobs(userID) {
		if j.cancel() {
			cancelled++
		} else {
			sending++
		}
	}
	switch {
	case cancelled > 0:
		// The job itself turns its status message into "cancelled".
	case sending > 0:
		b.send(chatID, textAlreadySent, nil)
	case b.session(userID) != nil:
		sess := b.session(userID)
		b.dropSession(userID)
		b.edit(chatID, sess.msgID, textCard(sess.info, sess.piece), nil)
		b.send(chatID, textDialogStopped, nil)
	default:
		b.send(chatID, textNothingToStop, nil)
	}
}

func (b *Bot) onCallback(ctx context.Context, cb *models.CallbackQuery) {
	userID := cb.From.ID
	answer := func(text string) {
		ctx, cancel := apiContext()
		defer cancel()
		if _, err := b.api.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: text}); err != nil {
			b.log.Debug("answer callback", "err", err)
		}
	}

	if level := b.access(userID); level == accessBanned || level == accessClosed {
		answer(textBanned)
		return
	}

	kind, rest, _ := strings.Cut(cb.Data, ":")
	id, arg, _ := strings.Cut(rest, ":")

	if kind == cbCancelJob {
		b.mu.Lock()
		j := b.jobs[id]
		b.mu.Unlock()
		switch {
		case j == nil || j.userID != userID:
			answer(textStale)
		case j.cancel():
			answer(textCancelling)
		default:
			answer(textAlreadySent)
		}
		return
	}

	sess := b.session(userID)
	if sess == nil || sess.id != id || cb.Message.Message == nil || cb.Message.Message.ID != sess.msgID {
		answer(textStale)
		return
	}
	answer("")
	b.onDialogButton(sess, kind, arg)
}

// send posts a message and returns its ID, 0 on failure.
func (b *Bot) send(chatID int64, text string, keyboard models.ReplyMarkup) int {
	ctx, cancel := apiContext()
	defer cancel()
	msg, err := b.api.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tg.True()},
		ReplyMarkup:        keyboard,
	})
	if err != nil {
		b.log.Warn("send message", "chat", chatID, "err", err)
		return 0
	}
	return msg.ID
}

// edit replaces the text and keyboard of a message.
func (b *Bot) edit(chatID int64, msgID int, text string, keyboard models.ReplyMarkup) {
	b.editPreview(chatID, msgID, text, keyboard, "")
}

// editPreview is edit with a link preview above the text — that is how the
// video card gets its picture while staying an editable text message.
func (b *Bot) editPreview(chatID int64, msgID int, text string, keyboard models.ReplyMarkup, previewURL string) {
	if msgID == 0 {
		b.send(chatID, text, keyboard)
		return
	}
	preview := &models.LinkPreviewOptions{IsDisabled: tg.True()}
	if previewURL != "" {
		preview = &models.LinkPreviewOptions{URL: &previewURL, ShowAboveText: tg.True()}
	}
	ctx, cancel := apiContext()
	defer cancel()
	_, err := b.api.EditMessageText(ctx, &tg.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          msgID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: preview,
		ReplyMarkup:        keyboard,
	})
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		b.log.Warn("edit message", "chat", chatID, "err", err)
	}
}

func (b *Bot) delete(chatID int64, msgID int) {
	if msgID == 0 {
		return
	}
	ctx, cancel := apiContext()
	defer cancel()
	if _, err := b.api.DeleteMessage(ctx, &tg.DeleteMessageParams{ChatID: chatID, MessageID: msgID}); err != nil {
		b.log.Debug("delete message", "chat", chatID, "err", err)
	}
}

// apiContext bounds a short Telegram call. It is deliberately detached from
// the update context: goodbye messages must go out while the bot shuts down.
func apiContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

type probeEntry struct {
	info *source.Info
	at   time.Time
}

const probeTTL = 10 * time.Minute

// probe fetches metadata, remembering it for a while: the same video is
// usually asked about again within the dialog or by the next person.
func (b *Bot) probe(ctx context.Context, src source.Source, link source.Link) (*source.Info, error) {
	key := link.Source + ":" + link.ID
	b.mu.Lock()
	entry, ok := b.probes[key]
	b.mu.Unlock()
	if ok && time.Since(entry.at) < probeTTL {
		return entry.info, nil
	}

	select {
	case b.probeSem <- struct{}{}:
		defer func() { <-b.probeSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	info, err := src.Probe(ctx, link)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	for k, e := range b.probes {
		if time.Since(e.at) >= probeTTL {
			delete(b.probes, k)
		}
	}
	b.probes[key] = probeEntry{info: info, at: time.Now()}
	b.mu.Unlock()
	return info, nil
}

// Shutdown drops waiting jobs (their owners are told about the restart),
// gives running ones until ctx is done, then stops them as well.
func (b *Bot) Shutdown(ctx context.Context) {
	b.queue.Shutdown(ctx)
}

func parseID(arg string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	return id, err == nil && id != 0
}

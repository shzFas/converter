package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/go-telegram/bot/models"

	"converter/internal/source"
	"converter/internal/timecode"
)

// step is where the user is in the dialog: link → format → quality →
// whole video or fragment → (typed timecodes) → job.
type step int

const (
	stepFormat step = iota + 1
	stepQuality
	stepRange
	stepTimecode
)

// Callback data is "<kind>:<session or job id>:<argument>".
const (
	cbFormat    = "f"
	cbQuality   = "q"
	cbRange     = "r"
	cbStop      = "x"
	cbCancelJob = "c"

	rangeAll  = "all"
	rangeFrom = "from"
	rangeCut  = "cut"
)

// formatText is a transcript: the source downloads MP3 audio and the bot
// turns the speech into text.
const formatText source.Format = "txt"

// piece is the part of the video to deliver.
type piece struct {
	start, end time.Duration
	whole      bool
}

func (p piece) length() time.Duration { return p.end - p.start }

// session is one user's unfinished dialog about one video. It is only
// touched while the user's lock is held.
type session struct {
	id     string
	userID int64
	chatID int64
	msgID  int // the card message that carries the buttons
	step   step

	src  source.Source
	link source.Link
	info *source.Info

	format source.Format
	tier   int
	piece  *piece // nil until the user has chosen

	timer *time.Timer // input timeout while timecodes are awaited
}

func (s *session) stopTimer() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// accepts reports whether a button of this kind belongs to the current step.
func (s *session) accepts(kind string) bool {
	switch kind {
	case cbStop:
		return true
	case cbFormat:
		return s.step == stepFormat
	case cbQuality:
		return s.step == stepQuality
	case cbRange:
		return s.step == stepRange
	}
	return false
}

func (s *session) card() string { return textCard(s.info, s.piece) }

func (b *Bot) session(userID int64) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[userID]
}

func (b *Bot) dropSession(userID int64) {
	b.mu.Lock()
	sess := b.sessions[userID]
	delete(b.sessions, userID)
	b.mu.Unlock()
	if sess != nil {
		sess.stopTimer()
	}
}

func newSessionID() string {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// startDialog probes the video and shows its card, or — with a preset
// format from /mp3 and /mp4 — goes straight to the queue.
func (b *Bot) startDialog(ctx context.Context, chatID, userID int64, src source.Source, link source.Link, timecodes string, preset source.Format) {
	var typed *timecode.Range
	if timecodes != "" {
		r, err := timecode.Parse(timecodes)
		if err != nil {
			b.send(chatID, textTimecodeError(err, 0), nil)
			return
		}
		typed = &r
	}

	// A new link replaces the unfinished dialog; its card loses the buttons.
	if old := b.session(userID); old != nil {
		b.dropSession(userID)
		b.edit(old.chatID, old.msgID, old.card(), nil)
	}

	msgID := b.send(chatID, textProbing, nil)
	info, err := b.probe(ctx, src, link)
	if err != nil {
		if ctx.Err() != nil {
			b.edit(chatID, msgID, textRestarting, nil)
			return
		}
		b.log.Warn("probe failed", "user", userID, "video", link.ID, "err", err)
		b.noteSourceResult(err)
		b.edit(chatID, msgID, textSourceError(err), nil)
		return
	}
	b.noteSourceResult(nil)

	switch {
	case b.store.VideoBlocked(info.ID):
		b.edit(chatID, msgID, textVideoBlocked, nil)
		return
	case info.Live || info.Duration < timecode.MinLength:
		b.edit(chatID, msgID, textLive, nil)
		return
	}

	sess := &session{
		id: newSessionID(), userID: userID, chatID: chatID, msgID: msgID,
		src: src, link: link, info: info,
	}
	if typed != nil {
		start, end, whole, err := typed.Resolve(info.Duration)
		if err != nil {
			b.edit(chatID, msgID, textTimecodeError(err, info.Duration), nil)
			return
		}
		sess.piece = &piece{start: start, end: end, whole: whole}
	}

	b.mu.Lock()
	b.sessions[userID] = sess
	b.mu.Unlock()

	if preset != "" {
		b.fastPath(sess, preset)
		return
	}
	b.showFormats(sess)
}

// problem tells why a piece of this length cannot be delivered, "" if it
// can, along with the estimated size.
func (b *Bot) problem(info *source.Info, format source.Format, tier int, length time.Duration) (string, int64) {
	size := info.EstimateSize(format, tier, length)
	switch {
	case length > b.cfg.MaxDuration:
		return textTooLong(b.cfg), size
	case size > b.cfg.MaxFileBytes:
		return textTooBig(size, b.cfg), size
	}
	return "", size
}

// maxLength is the longest fragment that passes both limits.
func (b *Bot) maxLength(info *source.Info, format source.Format, tier int) time.Duration {
	limit := min(b.cfg.MaxDuration, info.Duration)
	if whole := info.EstimateSize(format, tier, info.Duration); whole > b.cfg.MaxFileBytes {
		// 3% off so that the hint itself passes the check it comes from.
		bySize := float64(info.Duration) * float64(b.cfg.MaxFileBytes) / float64(whole) * 0.97
		limit = min(limit, time.Duration(bySize))
	}
	return limit.Truncate(time.Second)
}

func (b *Bot) wholePiece(info *source.Info) piece {
	return piece{start: 0, end: info.Duration, whole: true}
}

func button(text, kind, id, arg string) []models.InlineKeyboardButton {
	return []models.InlineKeyboardButton{{Text: text, CallbackData: kind + ":" + id + ":" + arg}}
}

func keyboard(rows ...[]models.InlineKeyboardButton) models.ReplyMarkup {
	if len(rows) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (b *Bot) showFormats(sess *session) {
	var row []models.InlineKeyboardButton
	if len(sess.info.Qualities) > 0 {
		row = append(row, button(btnMP4, cbFormat, sess.id, string(source.MP4))...)
	}
	row = append(row, button(btnMP3, cbFormat, sess.id, string(source.MP3))...)
	if b.cfg.Transcriber != nil {
		row = append(row, button(btnText, cbFormat, sess.id, string(formatText))...)
	}

	sess.step = stepFormat
	b.editPreview(sess.chatID, sess.msgID, textChooseFormat(sess.card()), keyboard(row), sess.link.URL)
}

// showQualities offers the video qualities. With a known fragment only the
// fitting ones are shown; otherwise the ones too big as a whole are marked,
// since a fragment of them may still fit.
func (b *Bot) showQualities(sess *session) {
	info := sess.info
	var rows [][]models.InlineKeyboardButton
	onlyFragment := false
	for _, q := range info.Qualities {
		length := info.Duration
		if sess.piece != nil {
			length = sess.piece.length()
		}
		problem, size := b.problem(info, source.MP4, q.Tier, length)
		label := fmt.Sprintf("%s · ≈%s", q.Label, formatSize(size))
		if problem != "" {
			if sess.piece != nil {
				continue
			}
			label = q.Label + " · " + btnFragmentOf
			onlyFragment = true
		}
		rows = append(rows, button(label, cbQuality, sess.id, strconv.Itoa(q.Tier)))
	}

	if len(rows) == 0 {
		b.showNothingFits(sess)
		return
	}
	sess.step = stepQuality
	b.editPreview(sess.chatID, sess.msgID, textChooseQuality(sess.card(), onlyFragment, b.cfg), keyboard(rows...), sess.link.URL)
}

// showNothingFits explains that the chosen fragment is too big as video in
// any quality and leaves MP3 as the way out.
func (b *Bot) showNothingFits(sess *session) {
	info := sess.info
	lowest := info.Qualities[0]
	length := sess.piece.length()
	problem, _ := b.problem(info, source.MP4, lowest.Tier, length)
	mp3Problem, _ := b.problem(info, source.MP3, 0, length)

	var rows [][]models.InlineKeyboardButton
	if mp3Problem == "" {
		rows = append(rows, button(btnMP3, cbFormat, sess.id, string(source.MP3)))
	}
	sess.step = stepFormat
	text := textNothingFits(sess.card(), problem, b.maxLength(info, source.MP4, lowest.Tier), mp3Problem == "")
	b.edit(sess.chatID, sess.msgID, text, keyboard(rows...))
}

// showRange asks: the whole video or a fragment.
func (b *Bot) showRange(sess *session) {
	info := sess.info
	var rows [][]models.InlineKeyboardButton

	wholeProblem, wholeSize := b.problem(info, sess.format, sess.tier, info.Duration)
	if wholeProblem == "" {
		rows = append(rows, button(rangeLabel("Весь ролик", sess.format, wholeSize), cbRange, sess.id, rangeAll))
	}
	if p, ok := b.pieceFromLink(sess); ok {
		if problem, size := b.problem(info, sess.format, sess.tier, p.length()); problem == "" {
			label := rangeLabel("С "+timecode.Format(p.start)+" до конца", sess.format, size)
			rows = append(rows, button(label, cbRange, sess.id, rangeFrom))
		}
	}
	rows = append(rows, button(btnCut, cbRange, sess.id, rangeCut))

	sess.step = stepRange
	text := textChooseRange(sess.card(), wholeProblem, b.maxLength(info, sess.format, sess.tier))
	b.editPreview(sess.chatID, sess.msgID, text, keyboard(rows...), sess.link.URL)
}

// rangeLabel adds the expected size, which means nothing for a transcript.
func rangeLabel(text string, format source.Format, size int64) string {
	if format == formatText {
		return text
	}
	return fmt.Sprintf("%s · ≈%s", text, formatSize(size))
}

// pieceFromLink is the fragment suggested by ?t=90 in the link.
func (b *Bot) pieceFromLink(sess *session) (piece, bool) {
	start, end, whole, err := timecode.From(sess.link.StartAt).Resolve(sess.info.Duration)
	if err != nil || whole {
		return piece{}, false
	}
	return piece{start: start, end: end}, true
}

// onDialogButton handles a button of the current step; the caller has
// already checked that the button belongs to it.
func (b *Bot) onDialogButton(sess *session, kind, arg string) {
	switch kind {
	case cbStop:
		b.dropSession(sess.userID)
		b.edit(sess.chatID, sess.msgID, sess.card(), nil)

	case cbFormat:
		switch source.Format(arg) {
		case source.MP3:
			sess.format, sess.tier = source.MP3, 0
			b.afterQuality(sess)
		case formatText:
			if b.cfg.Transcriber == nil {
				return
			}
			sess.format, sess.tier = formatText, 0
			b.afterQuality(sess)
		case source.MP4:
			if len(sess.info.Qualities) == 0 {
				return
			}
			sess.format = source.MP4
			b.showQualities(sess)
		}

	case cbQuality:
		tier, _ := strconv.Atoi(arg)
		for _, q := range sess.info.Qualities {
			if q.Tier == tier {
				sess.tier = tier
				b.afterQuality(sess)
				return
			}
		}

	case cbRange:
		switch arg {
		case rangeAll:
			whole := b.wholePiece(sess.info)
			sess.piece = &whole
			b.submit(sess)
		case rangeFrom:
			if p, ok := b.pieceFromLink(sess); ok {
				sess.piece = &p
				b.submit(sess)
			}
		case rangeCut:
			b.askTimecodes(sess)
		}
	}
}

// afterQuality skips the fragment question when timecodes came with the link.
func (b *Bot) afterQuality(sess *session) {
	if sess.piece != nil {
		b.submit(sess)
		return
	}
	b.showRange(sess)
}

func (b *Bot) askTimecodes(sess *session) {
	sess.step = stepTimecode
	text := textAskTimecode(sess.card(), b.maxLength(sess.info, sess.format, sess.tier))
	b.edit(sess.chatID, sess.msgID, text, keyboard(button(btnCancel, cbStop, sess.id, "")))
	b.armTimer(sess)
}

// armTimer (re)starts the input timeout: after it the dialog is dropped.
func (b *Bot) armTimer(sess *session) {
	sess.stopTimer()
	sess.timer = time.AfterFunc(b.cfg.InputTimeout, func() {
		unlock := b.lockUser(sess.userID)
		defer unlock()
		if b.session(sess.userID) != sess || sess.step != stepTimecode {
			return
		}
		b.dropSession(sess.userID)
		b.edit(sess.chatID, sess.msgID, textInputExpired, nil)
	})
}

// onTimecodes handles the message typed while timecodes are awaited. On any
// error the bot explains and keeps waiting.
func (b *Bot) onTimecodes(sess *session, text string) {
	info := sess.info
	retry := func(message string) {
		b.send(sess.chatID, message, nil)
		b.armTimer(sess)
	}

	typed, err := timecode.Parse(text)
	if err != nil {
		retry(textTimecodeError(err, info.Duration))
		return
	}
	start, end, whole, err := typed.Resolve(info.Duration)
	if err != nil {
		retry(textTimecodeError(err, info.Duration))
		return
	}
	p := piece{start: start, end: end, whole: whole}
	if problem, _ := b.problem(info, sess.format, sess.tier, p.length()); problem != "" {
		retry(textPieceProblem(problem, b.maxLength(info, sess.format, sess.tier)))
		return
	}
	sess.piece = &p
	b.submit(sess)
}

// fastPath serves /mp3, /mp4 and /text: no questions, 720p or the best below it
// that fits. Questions are asked only when the whole video is over the limits.
func (b *Bot) fastPath(sess *session, format source.Format) {
	info := sess.info
	p := b.wholePiece(info)
	if sess.piece != nil {
		p = *sess.piece
	}

	sess.format = format
	if format == source.MP4 {
		if len(info.Qualities) == 0 {
			b.dropSession(sess.userID)
			b.edit(sess.chatID, sess.msgID, textNoVideoStream, nil)
			return
		}
		found := false
		for i := len(info.Qualities) - 1; i >= 0 && !found; i-- {
			q := info.Qualities[i]
			if q.Tier > b.cfg.DefaultTier {
				continue
			}
			if problem, _ := b.problem(info, format, q.Tier, p.length()); problem == "" {
				sess.tier, found = q.Tier, true
			}
		}
		if !found && sess.piece == nil {
			// The whole video is too big even in the default quality: let the
			// user pick a quality and cut a fragment.
			b.showQualities(sess)
			return
		}
		if !found {
			b.showNothingFits(sess)
			return
		}
	} else if problem, _ := b.problem(info, format, 0, p.length()); problem != "" && sess.piece == nil {
		b.showRange(sess)
		return
	}
	sess.piece = &p
	b.submit(sess)
}

// submit ends the dialog and hands the choice over to the queue.
func (b *Bot) submit(sess *session) {
	b.dropSession(sess.userID)
	if problem, _ := b.problem(sess.info, sess.format, sess.tier, sess.piece.length()); problem != "" {
		limit := b.maxLength(sess.info, sess.format, sess.tier)
		b.edit(sess.chatID, sess.msgID, textNothingFits(sess.card(), problem, limit, false), nil)
		return
	}
	b.enqueue(sess)
}

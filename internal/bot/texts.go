package bot

import (
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"converter/internal/queue"
	"converter/internal/source"
	"converter/internal/timecode"
	"converter/internal/transcribe"
)

// All user-facing texts live in this file, so a second language is a second
// file and not a hunt through the handlers. Messages use Telegram HTML.

const (
	exampleLink     = "https://youtu.be/jNQXAC9IVRw"
	exampleTimecode = "1:30-2:45"

	btnMP4        = "🎬 MP4"
	btnMP3        = "🎵 MP3"
	btnText       = "📝 Текст"
	btnCut        = "✂️ Вырезать фрагмент"
	btnCancel     = "Отмена"
	btnFragmentOf = "только фрагмент"

	textProbing       = "🔎 Смотрю ролик…"
	textNoLink        = "Пришлите ссылку на видео с YouTube. Примеры и лимиты — /help"
	textNotSupported  = "Пока поддерживается только YouTube. Подойдут ссылки вида youtube.com/watch?v=…, youtu.be/… и youtube.com/shorts/…"
	textNotVideo      = "Это ссылка на YouTube, но не на отдельное видео. Плейлисты и каналы не поддерживаются — пришлите ссылку на сам ролик."
	textUnknownCmd    = "Не знаю такой команды. Пришлите ссылку на видео или загляните в /help"
	textStale         = "Кнопка устарела — пришлите ссылку ещё раз"
	textNothingToStop = "Сейчас нет активной задачи."
	textDialogStopped = "Хорошо, отменил. Пришлите ссылку, когда понадобится."
	textInputExpired  = "⌛ Не дождался таймкодов за 5 минут — задача сброшена. Пришлите ссылку ещё раз."
	textAlreadySent   = "Файл уже отправляется, отменить не получится"
	textCancelling    = "Отменяю…"
	textBanned        = "Доступ к боту закрыт."
	textVideoBlocked  = "Это видео недоступно в боте."
	textLive          = "Прямые эфиры и ещё не начавшиеся премьеры не поддерживаются. Попробуйте, когда трансляция закончится."
	textNoVideoStream = "У этого ролика нет видеодорожки — попробуйте MP3."
	textUserBusy      = "У вас уже есть активная задача. Дождитесь её или отмените: /cancel"
	textQueueFull     = "Очередь заполнена. Попробуйте через пару минут."
	textRestarting    = "♻️ Бот перезапускается. Пришлите ссылку ещё раз через минуту."
	textCancelled     = "❌ Отменено."
	textSendFailed    = "Не удалось отправить файл в Telegram. Попробуйте ещё раз."
	textInternal      = "Что-то пошло не так на моей стороне. Попробуйте ещё раз."
	textFastUsage     = "Формат: <code>/%s ссылка [таймкоды]</code>\nНапример: <code>/%s " + exampleLink + " 0:05-0:15</code>"
	textFastCutNote   = "<i>Быстрый рез: границы могли сдвинуться на пару секунд.</i>"
	textAdminUsage    = "Формат: <code>/%s ID</code>"
	textNoTranscriber = "Распознавание речи сейчас выключено."
	textNoSpeech      = "Речи в ролике не нашлось — только музыка или тишина."
	textTranscribeErr = "Не получилось распознать речь. Попробуйте ещё раз или возьмите фрагмент."
	textRestInFile    = "<i>Дальше — в файле.</i>"
)

func textStart() string {
	return "👋 Пришлите ссылку на видео с YouTube — верну MP4, MP3 или текст.\n\n" +
		"Фрагмент можно указать сразу:\n" +
		"<code>" + exampleLink + " 0:05-0:15</code>\n\n" +
		"Без кнопок:\n" +
		"<code>/mp3 ссылка [таймкоды]</code>\n" +
		"<code>/mp4 ссылка [таймкоды]</code> — в 720p\n" +
		"<code>/text ссылка [таймкоды]</code> — расшифровка речи\n\n" +
		"/help — форматы таймкодов и лимиты\n" +
		"/cancel — отменить текущую задачу\n\n" +
		"<i>Скачивайте только то, на что у вас есть права. Бот не хранит файлы и историю запросов.</i>"
}

func textHelp(cfg Config) string {
	return "<b>Как пользоваться</b>\n" +
		"1. Пришлите ссылку на видео или Shorts с YouTube.\n" +
		"2. Выберите MP4, MP3 или 📝 Текст, для видео — качество.\n" +
		"3. Скачайте весь ролик или вырежьте фрагмент.\n\n" +
		"<b>Таймкоды</b>\n" +
		"<code>1:30-2:45</code> — с 1:30 до 2:45\n" +
		"<code>90-165</code> — то же в секундах\n" +
		"<code>1:02:30-1:05:00</code> — часы:минуты:секунды\n" +
		"<code>1:30-</code> — с 1:30 до конца\n" +
		"<code>-2:45</code> — с начала до 2:45\n" +
		"Между началом и концом — дефис, тире или пробел.\n\n" +
		"<b>Лимиты</b>\n" +
		fmt.Sprintf("• файл до %s\n", formatSize(cfg.MaxFileBytes)) +
		fmt.Sprintf("• ролик или фрагмент до %s\n", formatMinutes(cfg.MaxDuration)) +
		fmt.Sprintf("• %d задач в сутки, по одной за раз\n", cfg.DailyQuota) +
		fmt.Sprintf("• фрагменты MP4 до %s режутся кадр в кадр, длиннее — по ключевым кадрам, границы могут сдвинуться на пару секунд\n\n", formatMinutes(cfg.PreciseCutMax)) +
		"Лимиты считаются по фрагменту: из трёхчасового ролика можно вырезать две минуты.\n\n" +
		"<b>Текст</b>\n" +
		"Речь распознаётся прямо на сервере, язык определяется сам. Текст придёт сообщениями и файлом .txt с таймкодами. Час речи — примерно 4–5 минут.\n\n" +
		"Не поддерживаются плейлисты, прямые эфиры, приватные видео и видео с возрастным ограничением."
}

func textClosed(userID int64) string {
	return fmt.Sprintf("Бот работает по списку доступа. Ваш ID: <code>%d</code> — передайте его администратору.", userID)
}

// textCard is the header of every dialog message: what video this is about.
func textCard(info *source.Info, p *piece) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(info.Title))
	if info.Channel != "" {
		fmt.Fprintf(&b, "%s · ", html.EscapeString(info.Channel))
	}
	b.WriteString(timecode.Format(info.Duration))
	if p != nil && !p.whole {
		fmt.Fprintf(&b, "\n✂️ %s", formatPiece(*p))
	}
	return b.String()
}

func textChooseFormat(card string) string { return card + "\n\nВыберите формат:" }

func textChooseQuality(card string, someOnlyFragment bool, cfg Config) string {
	text := card + "\n\nВыберите качество:"
	if someOnlyFragment {
		text += fmt.Sprintf("\n<i>Целиком ролик проходит по лимиту %s не во всех вариантах — из остальных можно вырезать фрагмент.</i>", formatSize(cfg.MaxFileBytes))
	}
	return text
}

func textChooseRange(card string, wholeProblem string, maxLen time.Duration) string {
	if wholeProblem == "" {
		return card + "\n\nЧто скачать?"
	}
	return fmt.Sprintf("%s\n\nЦеликом не получится: %s.\nМожно вырезать фрагмент примерно до %s.",
		card, wholeProblem, timecode.Format(maxLen))
}

func textAskTimecode(card string, maxLen time.Duration) string {
	return fmt.Sprintf("%s\n\nНапишите таймкоды сообщением, например <code>%s</code>.\n"+
		"Можно <code>1:30-</code> (до конца) или <code>-2:45</code> (с начала). Фрагмент — примерно до %s.\n\n"+
		"/cancel — передумал", card, exampleTimecode, timecode.Format(maxLen))
}

func textNothingFits(card, problem string, maxLen time.Duration, mp3 bool) string {
	text := fmt.Sprintf("%s\n\nНе получится: %s.", card, problem)
	if maxLen >= timecode.MinLength {
		text += fmt.Sprintf(" Поместится примерно %s.", timecode.Format(maxLen))
	}
	if mp3 {
		text += "\nМожно взять MP3 или прислать ссылку с фрагментом короче."
	} else {
		text += "\nПришлите ссылку с фрагментом короче."
	}
	return text
}

// textTimecodeError explains why the typed timecodes were not accepted.
func textTimecodeError(err error, duration time.Duration) string {
	switch {
	case errors.Is(err, timecode.ErrOrder):
		return "Начало должно быть раньше конца. Например: <code>" + exampleTimecode + "</code>"
	case errors.Is(err, timecode.ErrTooShort):
		return "Фрагмент должен быть не короче секунды. Например: <code>" + exampleTimecode + "</code>"
	case errors.Is(err, timecode.ErrStartBeyondEnd):
		return fmt.Sprintf("Начало за пределами ролика: его длительность %s.", timecode.Format(duration))
	default:
		return "Не понял таймкоды. Напишите начало и конец через дефис, например <code>" + exampleTimecode +
			"</code>, <code>90-165</code> или <code>1:30-</code>"
	}
}

func textPieceProblem(problem string, maxLen time.Duration) string {
	return fmt.Sprintf("Не получится: %s. Поместится примерно %s — пришлите таймкоды покороче.", problem, timecode.Format(maxLen))
}

// textTooLong and textTooBig are the reasons that follow "Не получится: ".
func textTooLong(cfg Config) string {
	return fmt.Sprintf("длительность больше %s", formatMinutes(cfg.MaxDuration))
}

func textTooBig(size int64, cfg Config) string {
	return fmt.Sprintf("файл выйдет около %s при лимите %s", formatSize(size), formatSize(cfg.MaxFileBytes))
}

func textQuota(cfg Config) string {
	return fmt.Sprintf("На сегодня лимит исчерпан: %d задач в сутки. Возвращайтесь завтра.", cfg.DailyQuota)
}

// textJobTitle describes the job in status messages and captions.
func textJobTitle(j *job) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(j.info.Title))
	switch j.req.Format {
	case formatText:
		b.WriteString(btnText)
	case source.MP3:
		b.WriteString("MP3")
	default:
		b.WriteString("MP4 " + j.quality.Label)
	}
	if !j.piece.whole {
		b.WriteString(" · ✂️ " + formatPiece(j.piece))
	}
	return b.String()
}

func textStatus(j *job, st stage, percent float64, position int) string {
	var line string
	switch st {
	case stageQueued:
		if position > 0 {
			line = fmt.Sprintf("⏳ В очереди, вы %d-й", position)
		} else {
			line = "⏳ Начинаю…"
		}
	case stageDownload:
		line = "⬇️ Скачиваю" + formatPercent(percent)
		if j.req.Format == formatText {
			line = "⬇️ Скачиваю звук" + formatPercent(percent)
		}
	case stageTranscribe:
		line = "📝 Распознаю речь" + formatPercent(percent)
	case stageConvert:
		line = "⚙️ Конвертирую" + formatPercent(percent)
	case stageSend:
		line = "📤 Отправляю"
	}
	return textJobTitle(j) + "\n\n" + line
}

func textCaption(j *job) string {
	var parts []string
	if j.req.Format != source.MP3 {
		parts = append(parts, "<b>"+html.EscapeString(truncate(j.info.Title, 200))+"</b>")
	}
	if !j.piece.whole {
		parts = append(parts, "✂️ "+formatPiece(j.piece))
		if j.req.Format == source.MP4 && !j.req.Precise {
			parts = append(parts, textFastCutNote)
		}
	}
	return strings.Join(parts, "\n")
}

// textJobError turns the reason a job has stopped into a message.
func textJobError(err error, cfg Config) string {
	var big *tooBigError
	switch {
	case errors.Is(err, queue.ErrCancelled):
		return textCancelled
	case errors.Is(err, queue.ErrShutdown):
		return textRestarting
	case errors.Is(err, queue.ErrTimeout):
		return fmt.Sprintf("⏱ Не успел за %s — задача остановлена. Попробуйте качество ниже или фрагмент короче.", formatMinutes(cfg.JobTimeout))
	case errors.As(err, &big):
		return fmt.Sprintf("Файл получился %s — больше лимита %s. Попробуйте качество ниже, MP3 или фрагмент короче.",
			formatSize(big.size), formatSize(cfg.MaxFileBytes))
	case errors.Is(err, errSend):
		return textSendFailed
	case errors.Is(err, errNoSpeech):
		return textNoSpeech
	case errors.Is(err, errTranscribe):
		return textTranscribeErr
	}
	return textSourceError(err)
}

// textSourceError explains a failed probe or download.
func textSourceError(err error) string {
	var srcErr *source.Error
	if !errors.As(err, &srcErr) {
		return textInternal
	}
	switch srcErr.Kind {
	case source.KindUnavailable:
		return "Видео недоступно: оно удалено или ссылка неверная."
	case source.KindPrivate:
		return "Это приватное видео — скачать его не получится."
	case source.KindAgeRestricted:
		return "У видео возрастное ограничение — YouTube отдаёт такие ролики только вошедшим в аккаунт, а у бота его нет."
	case source.KindLive:
		return textLive
	case source.KindMembersOnly:
		return "Это видео только для спонсоров канала — скачать его не получится."
	case source.KindGeo:
		return "Видео недоступно в стране, где работает сервер бота."
	case source.KindBlocked:
		return "YouTube временно ограничивает сервер бота. Попробуйте через несколько минут."
	default:
		return "Не удалось скачать видео. Возможно, YouTube что-то изменил — попробуйте позже."
	}
}

// textTranscriptFile is the content of the .txt: a short header, then one
// line per phrase with its time in the video.
func textTranscriptFile(j *job, res *transcribe.Result) string {
	var b strings.Builder
	b.WriteString(j.info.Title + "\n")
	if j.info.Channel != "" {
		b.WriteString(j.info.Channel + "\n")
	}
	b.WriteString(j.req.Link.URL + "\n")
	if !j.piece.whole {
		b.WriteString("Фрагмент: " + formatPiece(j.piece) + "\n")
	}
	if res.Language != "" {
		b.WriteString("Язык: " + res.Language + "\n")
	}
	b.WriteString("\n" + res.Timed(j.piece.start))
	return b.String()
}

func textAlert(failed, total int, window time.Duration) string {
	return fmt.Sprintf("⚠️ За последние %s упало %d из %d загрузок. Скорее всего, YouTube что-то изменил или ограничивает сервер — проверьте версию yt-dlp.",
		formatMinutes(window), failed, total)
}

func formatPiece(p piece) string {
	return fmt.Sprintf("%s–%s (%s)", timecode.Format(p.start), timecode.Format(p.end), timecode.Format(p.length()))
}

func formatPercent(percent float64) string {
	if percent < 0 {
		return "…"
	}
	return fmt.Sprintf(" %.0f%%", percent)
}

func formatSize(bytes int64) string {
	mb := float64(bytes) / (1 << 20)
	if mb < 10 {
		return fmt.Sprintf("%.1f МБ", mb)
	}
	return fmt.Sprintf("%.0f МБ", mb)
}

func formatMinutes(d time.Duration) string {
	return fmt.Sprintf("%d мин", int(d.Minutes()))
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

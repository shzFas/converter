package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"converter/internal/source"
	"converter/internal/transcribe"
)

// fakeTranscriber returns prepared segments without running whisper.
type fakeTranscriber struct {
	mu       sync.Mutex
	segments []transcribe.Segment
	err      error
	audio    []string
}

func (f *fakeTranscriber) Transcribe(_ context.Context, audio, _ string, progress func(float64)) (*transcribe.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = append(f.audio, audio)
	progress(50)
	if f.err != nil {
		return nil, f.err
	}
	return &transcribe.Result{Language: "ru", Segments: f.segments}, nil
}

func newTextHarness(t *testing.T, segments ...transcribe.Segment) (*harness, *fakeTranscriber) {
	t.Helper()
	if segments == nil {
		segments = []transcribe.Segment{
			{Start: time.Second, End: 3 * time.Second, Text: "Привет, это <тест>."},
			{Start: 3 * time.Second, End: 5 * time.Second, Text: "Вторая фраза."},
		}
	}
	tr := &fakeTranscriber{segments: segments}
	h := newHarness(t, func(c *Config) { c.Transcriber = tr })
	return h, tr
}

func (h *harness) waitDocs(n int) {
	h.t.Helper()
	h.eventually("sent documents", func() bool {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return len(h.api.docs) == n
	})
}

// chatTexts are the texts of all messages still in the chat.
func (h *harness) chatTexts() []string {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	var texts []string
	for _, id := range h.api.order {
		if !h.api.deleted[id] {
			texts = append(texts, h.api.texts[id])
		}
	}
	return texts
}

func TestTranscriptDialog(t *testing.T) {
	h, tr := newTextHarness(t)

	h.say(video)
	cardID, _ := h.api.last()
	h.press("Текст")
	h.expectLast("Что скачать?")
	for label := range h.api.buttons(cardID) {
		if strings.Contains(label, "МБ") {
			t.Errorf("button %q shows a size, meaningless for text", label)
		}
	}
	h.press("Весь ролик")
	h.waitDocs(1)
	h.waitIdle()

	if req := h.src.request(t, 0); req.Format != source.MP3 || !req.Whole() {
		t.Errorf("request = %+v, want whole MP3 audio", req)
	}
	if len(tr.audio) != 1 || !strings.HasSuffix(tr.audio[0], "media.mp3") {
		t.Errorf("transcribed %v, want the downloaded mp3", tr.audio)
	}

	texts := h.chatTexts()
	if len(texts) != 1 || texts[0] != "Привет, это &lt;тест&gt;. Вторая фраза." {
		t.Fatalf("chat = %q, want one escaped message with the text", texts)
	}

	doc := h.api.docData[0]
	for _, want := range []string{"Test <video>\n", "Язык: ru\n", "[0:01] Привет, это <тест>.\n", "[0:03] Вторая фраза.\n"} {
		if !strings.Contains(doc, want) {
			t.Errorf("file = %q, want %q in it", doc, want)
		}
	}
	if name := h.api.docs[0].Document.(*models.InputFileUpload).Filename; name != "Test video.txt" {
		t.Errorf("file name = %q", name)
	}
	if !h.api.isDeleted(cardID) || !h.tmpIsEmpty() {
		t.Error("status message or temporary files were left behind")
	}
}

func TestTranscriptOfFragmentKeepsVideoTime(t *testing.T) {
	h, _ := newTextHarness(t)

	h.say("/text " + video + " 1:30-2:45")
	h.waitDocs(1)
	h.waitIdle()

	if req := h.src.request(t, 0); req.Start != 90*time.Second || req.End != 165*time.Second {
		t.Errorf("request = %+v, want 90s-165s", req)
	}
	doc := h.api.docData[0]
	if !strings.Contains(doc, "[1:31] Привет") || !strings.Contains(doc, "Фрагмент: 1:30–2:45") {
		t.Errorf("file = %q, want times counted from the start of the video", doc)
	}
	if caption := h.api.docs[0].Caption; !strings.Contains(caption, "✂️ 1:30–2:45") {
		t.Errorf("caption = %q", caption)
	}
}

func TestLongTranscriptIsCutInChat(t *testing.T) {
	var segments []transcribe.Segment
	for i := range 400 {
		at := time.Duration(i) * 3 * time.Second
		segments = append(segments, transcribe.Segment{Start: at, End: at + 2*time.Second,
			Text: "Это довольно длинная фраза номер " + strings.Repeat("ля", 20) + "."})
	}
	h, _ := newTextHarness(t, segments...)

	h.say("/text " + video)
	h.waitDocs(1)
	h.waitIdle()

	texts := h.chatTexts()
	if len(texts) != chatMessages {
		t.Fatalf("chat has %d messages, want %d", len(texts), chatMessages)
	}
	for _, text := range texts {
		if n := len([]rune(text)); n > 4096 {
			t.Errorf("message of %d characters is over the Telegram limit", n)
		}
	}
	if !strings.HasSuffix(texts[len(texts)-1], textRestInFile) {
		t.Error("the last message does not point to the file")
	}
	if strings.Count(h.api.docData[0], "\n[") != 400 {
		t.Error("the file must hold the whole transcript")
	}
}

func TestTranscriptWithoutSpeech(t *testing.T) {
	h, tr := newTextHarness(t)
	tr.segments = []transcribe.Segment{}

	h.say("/text " + video)
	h.waitIdle()
	h.expectLast(textNoSpeech)
	if used := h.store.QuotaUsed(userID); used != 0 {
		t.Errorf("quota used = %d, want a refund", used)
	}

	tr.err = errors.New("whisper crashed")
	h.say("/text " + video)
	h.waitIdle()
	h.expectLast(textTranscribeErr)
}

func TestTranscriptIsCached(t *testing.T) {
	h, tr := newTextHarness(t)

	h.say("/text " + video)
	h.waitDocs(1)
	h.waitIdle()
	h.say("/text " + video)
	h.waitDocs(2)

	if n := h.src.downloads(); n != 1 || len(tr.audio) != 1 {
		t.Fatalf("downloads = %d, transcriptions = %d, want 1 each", n, len(tr.audio))
	}
	if cached, ok := h.api.docs[1].Document.(*models.InputFileString); !ok || cached.Data != "doc-1" {
		t.Fatalf("repeat was sent as %#v, want file_id doc-1", h.api.docs[1].Document)
	}
}

func TestTextOptionHiddenWithoutTranscriber(t *testing.T) {
	h := newHarness(t, nil)

	h.say(video)
	id, _ := h.api.last()
	for label := range h.api.buttons(id) {
		if strings.Contains(label, "Текст") {
			t.Fatalf("text button %q shown without a transcriber", label)
		}
	}
	h.say("/text " + video)
	h.expectLast(textNoTranscriber)
}

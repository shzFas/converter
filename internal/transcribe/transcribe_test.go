package transcribe

import (
	"strings"
	"testing"
	"time"
)

func TestParseJSON(t *testing.T) {
	data := []byte(`{
		"result": {"language": "ru"},
		"transcription": [
			{"offsets": {"from": 1060, "to": 4320}, "text": "  Я работаю в институте,"},
			{"offsets": {"from": 4400, "to": 6000}, "text": " [Музыка]"},
			{"offsets": {"from": 6000, "to": 7000}, "text": " ♪ ♪"},
			{"offsets": {"from": 9500, "to": 11000}, "text": " а   теперь непонятно."}
		]
	}`)
	res, err := parseJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Language != "ru" || len(res.Segments) != 2 {
		t.Fatalf("result = %+v, want ru and two segments without the sound labels", res)
	}
	if s := res.Segments[1]; s.Start != 9500*time.Millisecond || s.Text != "а теперь непонятно." {
		t.Errorf("segment = %+v", s)
	}
	if got := res.Plain(); got != "Я работаю в институте,\n\nа теперь непонятно." {
		t.Errorf("plain = %q, want a paragraph break at the long pause", got)
	}
	if got := res.Timed(time.Hour); got != "[1:00:01] Я работаю в институте,\n[1:00:09] а теперь непонятно.\n" {
		t.Errorf("timed = %q", got)
	}
}

func TestSplit(t *testing.T) {
	text := strings.Repeat("Слово слово слово. ", 50) + "\n\n" + strings.Repeat("ещё ", 100)
	chunks := Split(text, 300)
	if strings.Join(chunks, " ") == "" || len(chunks) < 4 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	total := 0
	for _, c := range chunks {
		if n := len([]rune(c)); n > 300 || n == 0 {
			t.Errorf("chunk of %d runes", n)
		}
		if strings.HasPrefix(c, " ") || strings.HasSuffix(c, " ") {
			t.Errorf("chunk %q is not trimmed", c)
		}
		total += len(strings.Fields(c))
	}
	if total != len(strings.Fields(text)) {
		t.Errorf("words lost: %d of %d", total, len(strings.Fields(text)))
	}
	if !strings.HasSuffix(chunks[0], ".") {
		t.Errorf("first chunk %q should end at a sentence", chunks[0])
	}
	// A word longer than the limit is cut anyway.
	if got := Split(strings.Repeat("я", 25), 10); len(got) != 3 {
		t.Errorf("long word split into %d chunks", len(got))
	}
}

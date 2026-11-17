package transcribe

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// paragraphPause is the silence that starts a new paragraph in plain text.
const paragraphPause = 2 * time.Second

// paragraphMax keeps paragraphs readable when the speaker never pauses.
const paragraphMax = 700

// Plain is the text without timecodes, split into paragraphs at pauses.
func (r *Result) Plain() string {
	var b strings.Builder
	para := 0
	for i, s := range r.Segments {
		switch {
		case i == 0:
		case para >= paragraphMax && endsSentence(r.Segments[i-1].Text),
			s.Start-r.Segments[i-1].End >= paragraphPause:
			b.WriteString("\n\n")
			para = 0
		default:
			b.WriteByte(' ')
		}
		b.WriteString(s.Text)
		para += utf8.RuneCountInString(s.Text)
	}
	return b.String()
}

// Timed is one line per phrase with its time in the video; offset is where
// the transcribed piece starts.
func (r *Result) Timed(offset time.Duration) string {
	var b strings.Builder
	for _, s := range r.Segments {
		fmt.Fprintf(&b, "[%s] %s\n", clock(offset+s.Start), s.Text)
	}
	return b.String()
}

func endsSentence(text string) bool {
	last, _ := utf8.DecodeLastRuneInString(text)
	return strings.ContainsRune(".!?…", last)
}

// clock formats 1:05 or 1:02:03.
func clock(d time.Duration) string {
	sec := int64(d / time.Second)
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// Split cuts text into chunks of at most limit runes, preferring paragraph,
// then sentence, then word boundaries.
func Split(text string, limit int) []string {
	var chunks []string
	for {
		text = strings.TrimSpace(text)
		if text == "" {
			return chunks
		}
		runes := []rune(text)
		if len(runes) <= limit {
			return append(chunks, text)
		}
		head := string(runes[:limit])
		cut := -1
		for _, sep := range []string{"\n\n", ". ", "! ", "? ", "… ", " "} {
			if i := strings.LastIndex(head, sep); i > len(head)/2 {
				cut = i + len(sep)
				break
			}
		}
		if cut < 0 {
			cut = len(head)
		}
		chunks = append(chunks, strings.TrimSpace(text[:cut]))
		text = text[cut:]
	}
}

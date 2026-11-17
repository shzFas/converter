// Package transcribe turns speech in an audio file into text with
// whisper.cpp, running entirely on this machine.
package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Segment is one phrase with its place in the audio.
type Segment struct {
	Start, End time.Duration
	Text       string
}

// Result is the recognized speech.
type Result struct {
	Language string // detected language code, e.g. "ru"; empty if unknown
	Segments []Segment
}

// Transcriber recognizes speech. It must stop its child processes when ctx
// is cancelled.
type Transcriber interface {
	// Transcribe reads audio (any format ffmpeg understands) and may use dir
	// for temporary files. progress gets 0–100.
	Transcribe(ctx context.Context, audio, dir string, progress func(percent float64)) (*Result, error)
}

// WhisperConfig tells where whisper.cpp and its models are.
type WhisperConfig struct {
	Binary   string // whisper-cli
	Model    string // ggml model file
	VADModel string // optional Silero VAD model: skips silence and music, less made-up text
	Language string // "auto" or a code such as "ru"
	Threads  int
	FFmpeg   string
}

// Whisper runs whisper-cli.
type Whisper struct{ cfg WhisperConfig }

func NewWhisper(cfg WhisperConfig) *Whisper {
	if cfg.Binary == "" {
		cfg.Binary = "whisper-cli"
	}
	if cfg.Language == "" {
		cfg.Language = "auto"
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 4
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	return &Whisper{cfg: cfg}
}

// whisper.cpp wants 16 kHz mono PCM.
func (w *Whisper) Transcribe(ctx context.Context, audio, dir string, progress func(float64)) (*Result, error) {
	if progress == nil {
		progress = func(float64) {}
	}
	wav := filepath.Join(dir, "speech.wav")
	args := []string{"-nostdin", "-v", "error", "-y", "-i", audio, "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav}
	if stderr, err := run(ctx, w.cfg.FFmpeg, args, nil); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("transcribe: ffmpeg: %v: %s", err, stderr)
	}

	// The language is detected first, so that the prompt matches it.
	lang := w.cfg.Language
	if lang == "auto" {
		detected, err := w.detect(ctx, wav)
		if err != nil {
			return nil, err
		}
		if detected != "" {
			lang = detected
		}
	}
	return w.recognize(ctx, wav, dir, lang, progress)
}

// prompts are a few punctuated sentences per language. Without them
// whisper often drifts into text with no punctuation at all, especially in
// Russian and when the audio starts mid-sentence.
var prompts = map[string]string{
	"ru": "Добрый день. Сегодня, как обычно, поговорим о главном.",
	"uk": "Добрий день. Сьогодні, як завжди, поговоримо про головне.",
	"be": "Добры дзень. Сёння, як звычайна, пагаворым пра галоўнае.",
	"en": "Hello, everyone. Today, as usual, we'll talk about the main thing.",
	"de": "Guten Tag. Heute sprechen wir, wie immer, über das Wichtigste.",
	"fr": "Bonjour à tous. Aujourd'hui, comme d'habitude, parlons de l'essentiel.",
	"es": "Hola a todos. Hoy, como siempre, hablaremos de lo más importante.",
}

// maxSegment keeps lines of the timed file short; whisper splits at words.
const maxSegment = 160

// detect asks whisper for the language of the first 30 seconds.
func (w *Whisper) detect(ctx context.Context, wav string) (string, error) {
	var lang string
	args := []string{"-m", w.cfg.Model, "-l", "auto", "-dl", "-t", strconv.Itoa(w.cfg.Threads), "-f", wav}
	stderr, err := run(ctx, w.cfg.Binary, args, func(line string) {
		if m := detectedLine.FindStringSubmatch(line); m != nil {
			lang = m[1]
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("transcribe: whisper language detection: %v: %s", err, stderr)
	}
	return lang, nil
}

var detectedLine = regexp.MustCompile(`auto-detected language: ([a-z]{2,3})\b`)

func (w *Whisper) recognize(ctx context.Context, wav, dir, lang string, progress func(float64)) (*Result, error) {
	out := filepath.Join(dir, "transcript")
	args := []string{
		"-m", w.cfg.Model,
		"-l", lang,
		"-t", strconv.Itoa(w.cfg.Threads),
		"-ml", strconv.Itoa(maxSegment), "-sow",
		"-pp", "-np", "-oj", "-of", out,
		"-f", wav,
	}
	if prompt := prompts[lang]; prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	if w.cfg.VADModel != "" {
		args = append(args, "--vad", "-vm", w.cfg.VADModel)
	}
	stderr, err := run(ctx, w.cfg.Binary, args, func(line string) {
		if m := progressLine.FindStringSubmatch(line); m != nil {
			p, _ := strconv.ParseFloat(m[1], 64)
			progress(p)
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("transcribe: whisper: %v: %s", err, stderr)
	}

	data, err := os.ReadFile(out + ".json")
	if err != nil {
		return nil, fmt.Errorf("transcribe: whisper left no result: %v: %s", err, stderr)
	}
	return parseJSON(data)
}

var progressLine = regexp.MustCompile(`progress\s*=\s*(\d+(?:\.\d+)?)%`)

// parseJSON reads the file whisper-cli writes with -oj.
func parseJSON(data []byte) (*Result, error) {
	var raw struct {
		Result struct {
			Language string `json:"language"`
		} `json:"result"`
		Transcription []struct {
			Offsets struct {
				From int64 `json:"from"`
				To   int64 `json:"to"`
			} `json:"offsets"`
			Text string `json:"text"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("transcribe: bad whisper output: %w", err)
	}
	res := &Result{Language: raw.Result.Language}
	for _, s := range raw.Transcription {
		text := strings.Join(strings.Fields(s.Text), " ")
		if text == "" || noise.MatchString(text) {
			continue
		}
		res.Segments = append(res.Segments, Segment{
			Start: time.Duration(s.Offsets.From) * time.Millisecond,
			End:   time.Duration(s.Offsets.To) * time.Millisecond,
			Text:  text,
		})
	}
	return res, nil
}

// noise matches segments that are only a sound label: "[Музыка]", "(applause)", "♪".
var noise = regexp.MustCompile(`^(\[[^\]]*\]|\([^)]*\)|[♪♫\s.…-])+$`)

// ErrNoModel means the model file is missing, so transcription is off.
var ErrNoModel = errors.New("transcribe: whisper model not found")

// Check verifies that the binary and the model are in place.
func (w *Whisper) Check() error {
	if _, err := os.Stat(w.cfg.Model); err != nil {
		return fmt.Errorf("%w: %s", ErrNoModel, w.cfg.Model)
	}
	if _, err := lookPath(w.cfg.Binary); err != nil {
		return fmt.Errorf("transcribe: %s not found: %w", w.cfg.Binary, err)
	}
	return nil
}

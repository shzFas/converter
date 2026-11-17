package timecode

import (
	"errors"
	"testing"
	"time"
)

func sec(n int) time.Duration { return time.Duration(n) * time.Second }

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Range
	}{
		{"1:30-2:45", Range{sec(90), sec(165), true, true}},
		{"90-165", Range{sec(90), sec(165), true, true}},
		{"1:02:30-1:05:00", Range{sec(3750), sec(3900), true, true}},
		{"1:30-", Range{Start: sec(90), HasStart: true}},
		{"-2:45", Range{End: sec(165), HasEnd: true}},
		{"1:30 2:45", Range{sec(90), sec(165), true, true}},
		{"1:30 - 2:45", Range{sec(90), sec(165), true, true}},
		{"1:30–2:45", Range{sec(90), sec(165), true, true}},
		{"1:30—2:45", Range{sec(90), sec(165), true, true}},
		{"  0-5 ", Range{0, sec(5), true, true}},
		{"90:00-95:00", Range{sec(5400), sec(5700), true, true}},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"", ErrSyntax},
		{"-", ErrSyntax},
		{"привет", ErrSyntax},
		{"1:30", ErrSyntax},
		{"1:30-2:45-3:00", ErrSyntax},
		{"1:75-2:00", ErrSyntax},
		{"1:2:3:4-5", ErrSyntax},
		{"1.5-2", ErrSyntax},
		{"1:-2", ErrSyntax},
		{"1 2 3", ErrSyntax},
		{"2:45-1:30", ErrOrder},
		{"1:30-1:30", ErrOrder},
		{"-0", ErrOrder},
	}
	for _, tt := range tests {
		if _, err := Parse(tt.in); !errors.Is(err, tt.want) {
			t.Errorf("Parse(%q) error = %v, want %v", tt.in, err, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	dur := sec(200)
	tests := []struct {
		in         string
		start, end time.Duration
		whole      bool
		err        error
	}{
		{"1:30-2:45", sec(90), sec(165), false, nil},
		{"1:30-", sec(90), sec(200), false, nil},
		{"-2:45", 0, sec(165), false, nil},
		{"1:30-10:00", sec(90), sec(200), false, nil}, // end is clamped silently
		{"0-10:00", 0, sec(200), true, nil},
		{"3:20-4:00", 0, 0, false, ErrStartBeyondEnd},
		{"5:00-", 0, 0, false, ErrStartBeyondEnd},
	}
	for _, tt := range tests {
		r, err := Parse(tt.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.in, err)
		}
		start, end, whole, err := r.Resolve(dur)
		if !errors.Is(err, tt.err) {
			t.Errorf("Resolve(%q) error = %v, want %v", tt.in, err, tt.err)
			continue
		}
		if start != tt.start || end != tt.end || whole != tt.whole {
			t.Errorf("Resolve(%q) = %v-%v whole=%v, want %v-%v whole=%v",
				tt.in, start, end, whole, tt.start, tt.end, tt.whole)
		}
	}
}

func TestFormat(t *testing.T) {
	tests := map[time.Duration]string{
		0:         "0:00",
		sec(5):    "0:05",
		sec(90):   "1:30",
		sec(3750): "1:02:30",
	}
	for in, want := range tests {
		if got := Format(in); got != want {
			t.Errorf("Format(%v) = %q, want %q", in, got, want)
		}
	}
}

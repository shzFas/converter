// Package timecode parses and validates fragment boundaries such as "1:30-2:45".
package timecode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MinLength is the shortest fragment the bot agrees to cut.
const MinLength = time.Second

var (
	// ErrSyntax means the input does not look like "start-end".
	ErrSyntax = errors.New("timecode: unrecognized input")
	// ErrOrder means start is not strictly before end.
	ErrOrder = errors.New("timecode: start must be before end")
	// ErrTooShort means the fragment is shorter than MinLength.
	ErrTooShort = errors.New("timecode: fragment is too short")
	// ErrStartBeyondEnd means start lies at or after the end of the video.
	ErrStartBeyondEnd = errors.New("timecode: start is beyond the video")
)

// Range is a fragment as the user typed it. A missing boundary means
// "from the very beginning" or "to the very end".
type Range struct {
	Start, End       time.Duration
	HasStart, HasEnd bool
}

// From returns an open-ended range starting at d.
func From(d time.Duration) Range {
	return Range{Start: d, HasStart: true}
}

var dashes = strings.NewReplacer("–", "-", "—", "-", "−", "-", "‒", "-", "―", "-")

// Parse reads "1:30-2:45", "90-165", "1:02:30-1:05:00", "1:30-" or "-2:45".
// Start and end may be separated by a hyphen, a dash or a space.
func Parse(s string) (Range, error) {
	s = strings.TrimSpace(dashes.Replace(s))
	if s == "" {
		return Range{}, ErrSyntax
	}

	var left, right string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		left, right = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	} else {
		fields := strings.Fields(s)
		if len(fields) != 2 {
			return Range{}, ErrSyntax
		}
		left, right = fields[0], fields[1]
	}
	if left == "" && right == "" {
		return Range{}, ErrSyntax
	}

	var r Range
	var err error
	if left != "" {
		if r.Start, err = parseTime(left); err != nil {
			return Range{}, err
		}
		r.HasStart = true
	}
	if right != "" {
		if r.End, err = parseTime(right); err != nil {
			return Range{}, err
		}
		r.HasEnd = true
	}
	if r.HasEnd && r.End <= r.Start {
		return Range{}, ErrOrder
	}
	if r.HasEnd && r.End-r.Start < MinLength {
		return Range{}, ErrTooShort
	}
	return r, nil
}

// parseTime reads SS, M:SS or H:MM:SS.
func parseTime(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, ErrSyntax
	}
	var total int64
	for i, p := range parts {
		if p == "" || len(p) > 6 {
			return 0, ErrSyntax
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, ErrSyntax
			}
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return 0, ErrSyntax
		}
		// Only the leading component may exceed 59: "90" and "90:00" are fine, "1:75" is not.
		if i > 0 && n > 59 {
			return 0, ErrSyntax
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second, nil
}

// Resolve checks the range against the video duration and returns concrete
// boundaries. An end beyond the video is silently clamped. whole reports that
// the result covers the entire video.
func (r Range) Resolve(duration time.Duration) (start, end time.Duration, whole bool, err error) {
	start, end = r.Start, r.End
	if !r.HasStart {
		start = 0
	}
	if !r.HasEnd || end > duration {
		end = duration
	}
	if start >= duration {
		return 0, 0, false, ErrStartBeyondEnd
	}
	if end-start < MinLength {
		return 0, 0, false, ErrTooShort
	}
	return start, end, start == 0 && end == duration, nil
}

// Format renders a duration as M:SS or H:MM:SS.
func Format(d time.Duration) string {
	total := int64(d.Round(time.Second) / time.Second)
	if total < 0 {
		total = 0
	}
	h, m, s := total/3600, total/60%60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

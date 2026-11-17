// Package store keeps the little state the bot needs between restarts: the
// file_id cache, daily quotas and the access lists. Everything lives in one
// JSON file; the volume is tiny, so the file is rewritten on every change.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// maxFiles bounds the file_id cache; the oldest entries go first.
const maxFiles = 5000

// File is a file already uploaded to Telegram.
type File struct {
	FileID string    `json:"file_id"`
	At     time.Time `json:"at"`
}

type data struct {
	Files map[string]File `json:"files"`
	// Quota is the number of tasks per user for the day in Day.
	Day   string          `json:"day"`
	Quota map[int64]int   `json:"quota"`
	Allow map[int64]bool  `json:"allow"`
	Deny  map[int64]bool  `json:"deny"`
	Video map[string]bool `json:"blocked_videos"`
}

// Store is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	d    data
	now  func() time.Time
}

// Open loads the file, creating an empty store if it does not exist yet.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.d); err != nil {
			return nil, fmt.Errorf("store %s is damaged: %w", path, err)
		}
	}
	if s.d.Files == nil {
		s.d.Files = map[string]File{}
	}
	if s.d.Quota == nil {
		s.d.Quota = map[int64]int{}
	}
	if s.d.Allow == nil {
		s.d.Allow = map[int64]bool{}
	}
	if s.d.Deny == nil {
		s.d.Deny = map[int64]bool{}
	}
	if s.d.Video == nil {
		s.d.Video = map[string]bool{}
	}
	return s, os.MkdirAll(filepath.Dir(path), 0o755)
}

// save writes the file atomically. The caller holds s.mu.
func (s *Store) save() error {
	raw, err := json.MarshalIndent(&s.d, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// File looks up an uploaded file by its cache key.
func (s *Store) File(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.d.Files[key]
	return f.FileID, ok
}

// PutFile remembers an uploaded file.
func (s *Store) PutFile(key, fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Files[key] = File{FileID: fileID, At: s.now()}
	if extra := len(s.d.Files) - maxFiles; extra > 0 {
		keys := make([]string, 0, len(s.d.Files))
		for k := range s.d.Files {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return s.d.Files[a].At.Compare(s.d.Files[b].At) })
		for _, k := range keys[:extra] {
			delete(s.d.Files, k)
		}
	}
	return s.save()
}

// DropFile forgets a file Telegram no longer accepts.
func (s *Store) DropFile(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.Files, key)
	return s.save()
}

// rollDay resets the quotas when the date changes. The caller holds s.mu.
func (s *Store) rollDay() {
	if today := s.now().Format(time.DateOnly); s.d.Day != today {
		s.d.Day = today
		s.d.Quota = map[int64]int{}
	}
}

// QuotaUsed is the number of tasks the user has started today.
func (s *Store) QuotaUsed(userID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	return s.d.Quota[userID]
}

// QuotaAdd changes today's counter of the user by delta (negative to refund).
func (s *Store) QuotaAdd(userID int64, delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	if n := s.d.Quota[userID] + delta; n > 0 {
		s.d.Quota[userID] = n
	} else {
		delete(s.d.Quota, userID)
	}
	return s.save()
}

// Allowed reports whether the user is on the allow-list, and whether the
// list has anyone on it at all.
func (s *Store) Allowed(userID int64) (allowed, listInUse bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Allow[userID], len(s.d.Allow) > 0
}

// Denied reports whether the user is on the deny-list.
func (s *Store) Denied(userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Deny[userID]
}

// SetAllowed adds the user to the allow-list or removes them from it.
func (s *Store) SetAllowed(userID int64, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set(s.d.Allow, userID, on)
	return s.save()
}

// SetDenied adds the user to the deny-list or removes them from it.
func (s *Store) SetDenied(userID int64, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set(s.d.Deny, userID, on)
	return s.save()
}

// VideoBlocked reports whether the video was blocked after a complaint.
func (s *Store) VideoBlocked(videoID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Video[videoID]
}

// SetVideoBlocked blocks or unblocks a video ID.
func (s *Store) SetVideoBlocked(videoID string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set(s.d.Video, videoID, on)
	return s.save()
}

// Counts is a summary for the admin.
type Counts struct {
	Files, UsersToday, TasksToday, Allowed, Denied, BlockedVideos int
}

func (s *Store) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay()
	c := Counts{
		Files:         len(s.d.Files),
		UsersToday:    len(s.d.Quota),
		Allowed:       len(s.d.Allow),
		Denied:        len(s.d.Deny),
		BlockedVideos: len(s.d.Video),
	}
	for _, n := range s.d.Quota {
		c.TasksToday += n
	}
	return c
}

func set[K comparable](m map[K]bool, key K, on bool) {
	if on {
		m[key] = true
	} else {
		delete(m, key)
	}
}

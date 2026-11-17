package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "store.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile("yt:abc|mp4|720|0|0", "FILE1"); err != nil {
		t.Fatal(err)
	}
	if err := s.QuotaAdd(42, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowed(42, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVideoBlocked("badvideo", true); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := s.File("yt:abc|mp4|720|0|0"); !ok || id != "FILE1" {
		t.Errorf("File = %q, %v", id, ok)
	}
	if got := s.QuotaUsed(42); got != 2 {
		t.Errorf("QuotaUsed = %d, want 2", got)
	}
	if allowed, inUse := s.Allowed(42); !allowed || !inUse {
		t.Errorf("Allowed = %v, %v", allowed, inUse)
	}
	if allowed, inUse := s.Allowed(7); allowed || !inUse {
		t.Errorf("Allowed(stranger) = %v, %v", allowed, inUse)
	}
	if !s.VideoBlocked("badvideo") {
		t.Error("blocked video was not saved")
	}

	if err := s.DropFile("yt:abc|mp4|720|0|0"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.File("yt:abc|mp4|720|0|0"); ok {
		t.Error("file is still cached after DropFile")
	}
}

func TestQuotaResetsNextDay(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 23, 59, 0, 0, time.Local)
	s.now = func() time.Time { return now }

	s.QuotaAdd(1, 1)
	s.QuotaAdd(1, 1)
	s.QuotaAdd(1, -1)
	if got := s.QuotaUsed(1); got != 1 {
		t.Fatalf("QuotaUsed = %d, want 1", got)
	}
	s.QuotaAdd(1, -5)
	if got := s.QuotaUsed(1); got != 0 {
		t.Fatalf("QuotaUsed after over-refund = %d, want 0", got)
	}

	s.QuotaAdd(1, 3)
	now = now.Add(2 * time.Minute)
	if got := s.QuotaUsed(1); got != 0 {
		t.Fatalf("QuotaUsed next day = %d, want 0", got)
	}
}

func TestDamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a damaged file")
	}
}

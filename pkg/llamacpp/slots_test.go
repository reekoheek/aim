package llamacpp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

func createAgedFile(t *testing.T, path string, age time.Duration, now time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("kv"), 0o644); err != nil {
		t.Fatal(err)
	}
	modified := now.Add(-age)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func remainingNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRemoveExpiredSlots(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		files         map[string]time.Duration
		dirs          []string
		expiration    time.Duration
		wantRemoved   []string
		wantRemaining []string
	}{
		{
			name:          "empty dir",
			expiration:    24 * time.Hour,
			wantRemoved:   nil,
			wantRemaining: nil,
		},
		{
			name:          "old slot removed, fresh slot kept",
			files:         map[string]time.Duration{"old.bin": 25 * time.Hour, "fresh.bin": time.Hour},
			expiration:    24 * time.Hour,
			wantRemoved:   []string{"old.bin"},
			wantRemaining: []string{"fresh.bin"},
		},
		{
			name:          "slot exactly at expiration kept",
			files:         map[string]time.Duration{"edge.bin": 24 * time.Hour},
			expiration:    24 * time.Hour,
			wantRemoved:   nil,
			wantRemaining: []string{"edge.bin"},
		},
		{
			name:          "short expiration",
			files:         map[string]time.Duration{"a.bin": 31 * time.Minute, "b.bin": 29 * time.Minute},
			expiration:    30 * time.Minute,
			wantRemoved:   []string{"a.bin"},
			wantRemaining: []string{"b.bin"},
		},
		{
			name:          "non-slot files kept",
			files:         map[string]time.Duration{"old.bin": 48 * time.Hour, "notes.txt": 48 * time.Hour},
			expiration:    24 * time.Hour,
			wantRemoved:   []string{"old.bin"},
			wantRemaining: []string{"notes.txt"},
		},
		{
			name:          "directories kept",
			dirs:          []string{"nested.bin"},
			expiration:    time.Nanosecond,
			wantRemoved:   nil,
			wantRemaining: []string{"nested.bin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, age := range tt.files {
				createAgedFile(t, filepath.Join(dir, name), age, now)
			}
			for _, name := range tt.dirs {
				if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			removed, err := removeExpiredSlots(dir, tt.expiration, now)
			if err != nil {
				t.Fatalf("removeExpiredSlots: %v", err)
			}
			if !slices.Equal(removed, tt.wantRemoved) {
				t.Errorf("removed %q, want %q", removed, tt.wantRemoved)
			}
			if remaining := remainingNames(t, dir); !slices.Equal(remaining, tt.wantRemaining) {
				t.Errorf("remaining %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestRemoveExpiredSlotsMissingDir(t *testing.T) {
	removed, err := removeExpiredSlots(filepath.Join(t.TempDir(), "missing"), time.Hour, time.Now())
	if err != nil || removed != nil {
		t.Errorf("got %q, %v, want no removals and no error", removed, err)
	}
}

func TestExpireSlots(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]time.Duration
		expiration    time.Duration
		wantRemaining []string
	}{
		{
			name:          "sweeps on start and stops when canceled",
			files:         map[string]time.Duration{"old.bin": 48 * time.Hour, "fresh.bin": 0},
			expiration:    24 * time.Hour,
			wantRemaining: []string{"fresh.bin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			for name, age := range tt.files {
				createAgedFile(t, filepath.Join(dir, name), age, now)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				ExpireSlots(ctx, dir, tt.expiration, logger.New("ERROR"))
			}()

			deadline := time.Now().Add(2 * time.Second)
			for !slices.Equal(remainingNames(t, dir), tt.wantRemaining) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("ExpireSlots did not stop after cancel")
			}
			if remaining := remainingNames(t, dir); !slices.Equal(remaining, tt.wantRemaining) {
				t.Errorf("remaining %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

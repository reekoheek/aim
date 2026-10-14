package llamaserver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
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

func TestPruneSlots(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		files         map[string]time.Duration
		dirs          []string
		expiration    time.Duration
		maxFiles      int
		wantPruned    []prunedSlot
		wantRemaining []string
	}{
		{
			name:          "empty dir",
			expiration:    time.Hour,
			maxFiles:      3,
			wantPruned:    nil,
			wantRemaining: nil,
		},
		{
			name:          "old slot expired, fresh slot kept",
			files:         map[string]time.Duration{"old.bin": 61 * time.Minute, "fresh.bin": time.Minute},
			expiration:    time.Hour,
			maxFiles:      3,
			wantPruned:    []prunedSlot{{name: "old.bin", reason: slotPruneReasonExpiry}},
			wantRemaining: []string{"fresh.bin"},
		},
		{
			name:          "slot exactly at expiration kept",
			files:         map[string]time.Duration{"edge.bin": time.Hour},
			expiration:    time.Hour,
			maxFiles:      3,
			wantPruned:    nil,
			wantRemaining: []string{"edge.bin"},
		},
		{
			name: "least recently used slots over the limit removed",
			files: map[string]time.Duration{
				"a.bin": 1 * time.Minute,
				"b.bin": 4 * time.Minute,
				"c.bin": 2 * time.Minute,
				"d.bin": 5 * time.Minute,
				"e.bin": 3 * time.Minute,
			},
			expiration: time.Hour,
			maxFiles:   3,
			wantPruned: []prunedSlot{
				{name: "b.bin", reason: slotPruneReasonLimit},
				{name: "d.bin", reason: slotPruneReasonLimit},
			},
			wantRemaining: []string{"a.bin", "c.bin", "e.bin"},
		},
		{
			name: "expiry and limit together",
			files: map[string]time.Duration{
				"a.bin": 1 * time.Minute,
				"b.bin": 2 * time.Minute,
				"c.bin": 90 * time.Minute,
			},
			expiration: time.Hour,
			maxFiles:   1,
			wantPruned: []prunedSlot{
				{name: "b.bin", reason: slotPruneReasonLimit},
				{name: "c.bin", reason: slotPruneReasonExpiry},
			},
			wantRemaining: []string{"a.bin"},
		},
		{
			name:          "zero expiration and limit keep everything",
			files:         map[string]time.Duration{"a.bin": 48 * time.Hour, "b.bin": 72 * time.Hour},
			expiration:    0,
			maxFiles:      0,
			wantPruned:    nil,
			wantRemaining: []string{"a.bin", "b.bin"},
		},
		{
			name:          "non-slot files ignored",
			files:         map[string]time.Duration{"old.bin": 2 * time.Hour, "notes.txt": 2 * time.Hour},
			expiration:    time.Hour,
			maxFiles:      3,
			wantPruned:    []prunedSlot{{name: "old.bin", reason: slotPruneReasonExpiry}},
			wantRemaining: []string{"notes.txt"},
		},
		{
			name:          "directories ignored",
			dirs:          []string{"nested.bin"},
			expiration:    time.Nanosecond,
			maxFiles:      1,
			wantPruned:    nil,
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

			pruned, err := pruneSlots(dir, tt.expiration, tt.maxFiles, now)
			if err != nil {
				t.Fatalf("pruneSlots: %v", err)
			}
			if !slices.Equal(pruned, tt.wantPruned) {
				t.Errorf("pruned %+v, want %+v", pruned, tt.wantPruned)
			}
			if remaining := remainingNames(t, dir); !slices.Equal(remaining, tt.wantRemaining) {
				t.Errorf("remaining %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestPruneSlotsMissingDir(t *testing.T) {
	pruned, err := pruneSlots(filepath.Join(t.TempDir(), "missing"), time.Hour, 3, time.Now())
	if err != nil || pruned != nil {
		t.Errorf("got %+v, %v, want nothing pruned and no error", pruned, err)
	}
}

func TestTouchSlotFile(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		exists bool
	}{
		{name: "existing file gets the new time", exists: true},
		{name: "missing file is not an error", exists: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.bin")
			if tt.exists {
				createAgedFile(t, path, 2*time.Hour, now)
			}
			if err := touchSlotFile(path, now); err != nil {
				t.Fatalf("touchSlotFile: %v", err)
			}
			if !tt.exists {
				return
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(now) {
				t.Errorf("modified %v, want %v", info.ModTime(), now)
			}
		})
	}
}

func TestPruneSlotsLoop(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]time.Duration
		server        config.ServerConfig
		wantRemaining []string
	}{
		{
			name:          "prunes on start and stops when canceled",
			files:         map[string]time.Duration{"old.bin": 2 * time.Hour, "a.bin": time.Minute, "b.bin": 2 * time.Minute},
			server:        config.ServerConfig{SlotExpiration: time.Hour, SlotMaxFiles: 1},
			wantRemaining: []string{"a.bin"},
		},
		{
			name:          "zero expiration does not stop the loop from running",
			files:         map[string]time.Duration{"a.bin": time.Minute, "b.bin": 2 * time.Minute},
			server:        config.ServerConfig{SlotMaxFiles: 1},
			wantRemaining: []string{"a.bin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			for name, age := range tt.files {
				createAgedFile(t, filepath.Join(dir, name), age, now)
			}
			server := tt.server
			server.SaveSlotPath = dir

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				PruneSlots(ctx, server, logger.New("ERROR"))
			}()

			deadline := time.Now().Add(2 * time.Second)
			for !slices.Equal(remainingNames(t, dir), tt.wantRemaining) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("PruneSlots did not stop after cancel")
			}
			if remaining := remainingNames(t, dir); !slices.Equal(remaining, tt.wantRemaining) {
				t.Errorf("remaining %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

package llamaserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

func runningProcess() <-chan struct{} {
	return make(chan struct{})
}

func exitedProcess() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

type fakeSlotServer struct {
	mu          sync.Mutex
	dir         string
	failRestore map[string]bool
	actions     []string
}

func (f *fakeSlotServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := r.URL.Query().Get("action")
	slotID := strings.TrimPrefix(r.URL.Path, "/slots/")

	f.mu.Lock()
	f.actions = append(f.actions, action+" "+slotID+" "+body.Filename)
	f.mu.Unlock()

	switch {
	case action == "restore" && f.failRestore[body.Filename]:
		http.Error(w, "restore failed", http.StatusInternalServerError)
	case action == "save":
		path := filepath.Join(f.dir, body.Filename)
		if err := os.WriteFile(path, []byte("kv"), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		savedAt := time.Now()
		if err := os.Chtimes(path, savedAt, savedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"n_saved": 1}`)
	default:
		fmt.Fprint(w, `{"n_restored": 1}`)
	}
}

func (f *fakeSlotServer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actions)
}

func newTestSlotCache(t *testing.T, slotCount int, existing []string, failRestore []string, maxFiles int, done <-chan struct{}) (*slotCache, *fakeSlotServer) {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeSlotServer{dir: dir, failRestore: map[string]bool{}}
	httpServer := httptest.NewServer(fake)
	t.Cleanup(httpServer.Close)

	server := config.ServerConfig{SlotExpiration: time.Hour, SlotMaxFiles: maxFiles}
	cache := newSlotCache(dir, "m", httpServer.URL, slotCount, server, logger.New("ERROR"), done)
	for _, session := range existing {
		if err := os.WriteFile(filepath.Join(dir, cache.file(session)), []byte("kv"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range failRestore {
		fake.failRestore[cache.file(session)] = true
	}
	return cache, fake
}

func runSlotRequests(t *testing.T, cache *slotCache, sessions []string) []int {
	t.Helper()
	var slotIDs []int
	for _, session := range sessions {
		slotID, release, err := cache.acquire(context.Background(), session)
		if err != nil {
			t.Fatalf("acquire(%q): %v", session, err)
		}
		release()
		slotIDs = append(slotIDs, slotID)
	}
	return slotIDs
}

func holdSlot(t *testing.T, cache *slotCache, session string) func() {
	t.Helper()
	_, release, err := cache.acquire(context.Background(), session)
	if err != nil {
		t.Fatalf("acquire(%q): %v", session, err)
	}
	return release
}

type slotGrant struct {
	slotID  int
	release func()
	err     error
}

func acquireSlotAsync(ctx context.Context, cache *slotCache, session string) <-chan slotGrant {
	granted := make(chan slotGrant, 1)
	go func() {
		slotID, release, err := cache.acquire(ctx, session)
		granted <- slotGrant{slotID: slotID, release: release, err: err}
	}()
	return granted
}

func queueSlotRequest(t *testing.T, cache *slotCache, session string) <-chan slotGrant {
	t.Helper()
	queuedBefore := waitingRequests(cache)
	granted := acquireSlotAsync(context.Background(), cache, session)
	waitUntil(t, time.Second, "request for "+session+" queued", func() bool {
		return waitingRequests(cache) == queuedBefore+1
	})
	return granted
}

func expectNoGrant(t *testing.T, granted <-chan slotGrant, what string) {
	t.Helper()
	select {
	case grant := <-granted:
		t.Fatalf("%s got slot %d (error %v) while it should wait", what, grant.slotID, grant.err)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitingRequests(cache *slotCache) int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.waiters)
}

func busySlots(cache *slotCache) int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	busy := 0
	for _, slot := range cache.slots {
		if slot.busy {
			busy++
		}
	}
	return busy
}

func expectedActions(cache *slotCache, actions []string) []string {
	var expected []string
	for _, action := range actions {
		parts := strings.SplitN(action, " ", 3)
		expected = append(expected, parts[0]+" "+parts[1]+" "+cache.file(parts[2]))
	}
	return expected
}

func TestSlotCacheAcquireSavesAndRestoresOnlyOnSessionSwitch(t *testing.T) {
	tests := []struct {
		name        string
		existing    []string
		failRestore []string
		requests    []string
		want        []string
	}{
		{
			name:     "same session reuses the slot without save or restore",
			requests: []string{"a", "a", "a"},
			want:     nil,
		},
		{
			name:     "switching session saves the previous one",
			requests: []string{"a", "b"},
			want:     []string{"save 0 a"},
		},
		{
			name:     "switching back restores the saved session",
			requests: []string{"a", "b", "a"},
			want:     []string{"save 0 a", "save 0 b", "restore 0 a"},
		},
		{
			name:     "existing cache is restored on the first request",
			existing: []string{"a"},
			requests: []string{"a", "a"},
			want:     []string{"restore 0 a"},
		},
		{
			name:        "session is saved after a failed restore",
			existing:    []string{"a"},
			failRestore: []string{"a"},
			requests:    []string{"a", "b"},
			want:        []string{"restore 0 a", "save 0 a"},
		},
		{
			name:     "session-less request evicts the session",
			requests: []string{"a", "", "a"},
			want:     []string{"save 0 a", "restore 0 a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, fake := newTestSlotCache(t, 1, tt.existing, tt.failRestore, 0, runningProcess())
			runSlotRequests(t, cache, tt.requests)
			if got, want := fake.recorded(), expectedActions(cache, tt.want); !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestSlotCacheAcquireSpreadsSessionsOverSlots(t *testing.T) {
	tests := []struct {
		name      string
		slotCount int
		existing  []string
		requests  []string
		wantSlots []int
		want      []string
	}{
		{
			name:      "each session keeps its own slot",
			slotCount: 2,
			requests:  []string{"a", "b", "a", "b"},
			wantSlots: []int{0, 1, 0, 1},
			want:      nil,
		},
		{
			name:      "new session evicts the least recently used slot",
			slotCount: 2,
			requests:  []string{"a", "b", "c"},
			wantSlots: []int{0, 1, 0},
			want:      []string{"save 0 a"},
		},
		{
			name:      "returning session refreshes its slot before eviction",
			slotCount: 2,
			requests:  []string{"a", "b", "a", "c"},
			wantSlots: []int{0, 1, 0, 1},
			want:      []string{"save 1 b"},
		},
		{
			name:      "evicted session is restored into the least recently used slot",
			slotCount: 2,
			requests:  []string{"a", "b", "c", "a"},
			wantSlots: []int{0, 1, 0, 1},
			want:      []string{"save 0 a", "save 1 b", "restore 1 a"},
		},
		{
			name:      "session-less request uses an empty slot",
			slotCount: 2,
			requests:  []string{"a", "", "a"},
			wantSlots: []int{0, 1, 0},
			want:      nil,
		},
		{
			name:      "empty slot is used before evicting a session",
			slotCount: 3,
			existing:  []string{"c"},
			requests:  []string{"a", "b", "a", "c"},
			wantSlots: []int{0, 1, 0, 2},
			want:      []string{"restore 2 c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, fake := newTestSlotCache(t, tt.slotCount, tt.existing, nil, 0, runningProcess())
			if got := runSlotRequests(t, cache, tt.requests); !slices.Equal(got, tt.wantSlots) {
				t.Errorf("slots %v, want %v", got, tt.wantSlots)
			}
			if got, want := fake.recorded(), expectedActions(cache, tt.want); !slices.Equal(got, want) {
				t.Errorf("actions %q, want %q", got, want)
			}
		})
	}
}

func TestSlotCacheAcquireWhileSlotsAreHeld(t *testing.T) {
	tests := []struct {
		name      string
		slotCount int
		held      []string
		next      string
		wantWait  bool
		wantSlot  int
	}{
		{
			name:      "other session runs in parallel on a free slot",
			slotCount: 2,
			held:      []string{"a"},
			next:      "b",
			wantSlot:  1,
		},
		{
			name:      "session-less requests run in parallel",
			slotCount: 2,
			held:      []string{""},
			next:      "",
			wantSlot:  1,
		},
		{
			name:      "same session waits for its own slot while another is free",
			slotCount: 2,
			held:      []string{"a"},
			next:      "a",
			wantWait:  true,
			wantSlot:  0,
		},
		{
			name:      "request waits while every slot is busy",
			slotCount: 1,
			held:      []string{"a"},
			next:      "b",
			wantWait:  true,
			wantSlot:  0,
		},
		{
			name:      "request takes the first slot released",
			slotCount: 2,
			held:      []string{"a", "b"},
			next:      "c",
			wantWait:  true,
			wantSlot:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, _ := newTestSlotCache(t, tt.slotCount, nil, nil, 0, runningProcess())
			var releases []func()
			for _, session := range tt.held {
				releases = append(releases, holdSlot(t, cache, session))
			}

			var granted <-chan slotGrant
			if tt.wantWait {
				granted = queueSlotRequest(t, cache, tt.next)
				expectNoGrant(t, granted, "request for "+tt.next)
				releases[0]()
				releases = releases[1:]
			} else {
				granted = acquireSlotAsync(context.Background(), cache, tt.next)
			}

			grant := receiveWithin(t, granted, time.Second, "request for "+tt.next)
			if grant.err != nil {
				t.Fatal(grant.err)
			}
			grant.release()
			for _, release := range releases {
				release()
			}
			if grant.slotID != tt.wantSlot {
				t.Errorf("slot %d, want %d", grant.slotID, tt.wantSlot)
			}
		})
	}
}

func TestSlotCacheHandsReleasedSlotToWaitingRequests(t *testing.T) {
	tests := []struct {
		name        string
		queued      []string
		wantOrder   []string
		wantActions []string
	}{
		{
			name:        "waiting requests are served in arrival order",
			queued:      []string{"b", "c"},
			wantOrder:   []string{"b", "c"},
			wantActions: []string{"save 0 a", "save 0 b"},
		},
		{
			name:        "session waiting for its own slot goes before earlier requests",
			queued:      []string{"b", "a"},
			wantOrder:   []string{"a", "b"},
			wantActions: []string{"save 0 a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, fake := newTestSlotCache(t, 1, nil, nil, 0, runningProcess())
			release := holdSlot(t, cache, "a")
			grants := map[string]<-chan slotGrant{}
			for _, session := range tt.queued {
				grants[session] = queueSlotRequest(t, cache, session)
			}

			for i, session := range tt.wantOrder {
				release()
				grant := receiveWithin(t, grants[session], time.Second, "request for "+session)
				if grant.err != nil {
					t.Fatal(grant.err)
				}
				for _, later := range tt.wantOrder[i+1:] {
					expectNoGrant(t, grants[later], "request for "+later)
				}
				release = grant.release
			}
			release()

			if got, want := fake.recorded(), expectedActions(cache, tt.wantActions); !slices.Equal(got, want) {
				t.Errorf("actions %q, want %q", got, want)
			}
		})
	}
}

func TestSlotCacheAbandonedRequestLeavesSlotUntouched(t *testing.T) {
	tests := []struct {
		name    string
		abandon func(t *testing.T, cache *slotCache)
	}{
		{
			name: "canceled while waiting",
			abandon: func(t *testing.T, cache *slotCache) {
				release := holdSlot(t, cache, "a")
				defer release()
				ctx, cancel := context.WithCancel(context.Background())
				queuedBefore := waitingRequests(cache)
				granted := acquireSlotAsync(ctx, cache, "b")
				waitUntil(t, time.Second, "request for b queued", func() bool {
					return waitingRequests(cache) == queuedBefore+1
				})
				cancel()
				if grant := receiveWithin(t, granted, time.Second, "canceled request"); !errors.Is(grant.err, context.Canceled) {
					t.Errorf("error %v, want %v", grant.err, context.Canceled)
				}
			},
		},
		{
			name: "given up after the slot was assigned",
			abandon: func(t *testing.T, cache *slotCache) {
				runSlotRequests(t, cache, []string{"a"})
				waiter := &slotWaiter{session: "b", assigned: make(chan slotAssignment, 1)}
				cache.mu.Lock()
				cache.waiters = append(cache.waiters, waiter)
				cache.dispatch()
				cache.mu.Unlock()
				cache.abandon(waiter)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, fake := newTestSlotCache(t, 1, nil, nil, 0, runningProcess())
			tt.abandon(t, cache)

			slot := cache.slots[0]
			if slot.session != "a" || !slot.unsaved || slot.busy {
				t.Errorf("slot session %q unsaved %v busy %v, want %q unsaved and free", slot.session, slot.unsaved, slot.busy, "a")
			}
			if queued := waitingRequests(cache); queued != 0 {
				t.Errorf("%d requests still waiting", queued)
			}
			if got := fake.recorded(); got != nil {
				t.Errorf("actions %q, want none", got)
			}
		})
	}
}

func TestSlotCacheAcquireGivesUpWhileSlotIsHeld(t *testing.T) {
	tests := []struct {
		name     string
		process  <-chan struct{}
		canceled bool
		want     error
	}{
		{name: "process exited", process: exitedProcess(), want: errServerExited},
		{name: "request canceled", process: runningProcess(), canceled: true, want: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, _ := newTestSlotCache(t, 1, nil, nil, 0, tt.process)
			cache.slots[0].busy = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.canceled {
				cancel()
			}

			if _, _, err := cache.acquire(ctx, "a"); !errors.Is(err, tt.want) {
				t.Errorf("error %v, want %v", err, tt.want)
			}
			if queued := waitingRequests(cache); queued != 0 {
				t.Errorf("%d requests still waiting", queued)
			}
		})
	}
}

func TestSlotCacheShutdownSavesUnsavedSlots(t *testing.T) {
	tests := []struct {
		name          string
		slotCount     int
		requests      []string
		processExited bool
		want          []string
	}{
		{
			name:      "unsaved session is saved",
			slotCount: 1,
			requests:  []string{"a"},
			want:      []string{"save 0 a"},
		},
		{
			name:      "session-less request leaves nothing to save",
			slotCount: 1,
			requests:  []string{""},
			want:      nil,
		},
		{
			name:      "session saved on switch is not saved again",
			slotCount: 1,
			requests:  []string{"a", "b"},
			want:      []string{"save 0 a", "save 0 b"},
		},
		{
			name:      "every slot with an unsaved session is saved",
			slotCount: 3,
			requests:  []string{"a", "b", "a"},
			want:      []string{"save 0 a", "save 1 b"},
		},
		{
			name:          "exited process is not asked to save",
			slotCount:     1,
			requests:      []string{"a"},
			processExited: true,
			want:          nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			process := make(chan struct{})
			cache, fake := newTestSlotCache(t, tt.slotCount, nil, nil, 0, process)
			runSlotRequests(t, cache, tt.requests)
			if tt.processExited {
				close(process)
			}

			cache.shutdown(context.Background())

			if got, want := fake.recorded(), expectedActions(cache, tt.want); !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestSlotCacheShutdownWaitsForEverySlot(t *testing.T) {
	cache, fake := newTestSlotCache(t, 2, nil, nil, 0, runningProcess())
	releaseA := holdSlot(t, cache, "a")
	releaseB := holdSlot(t, cache, "b")

	shutdown := make(chan struct{})
	go func() {
		cache.shutdown(context.Background())
		close(shutdown)
	}()

	releaseA()
	select {
	case <-shutdown:
		t.Fatal("shutdown finished while a request still held a slot")
	case <-time.After(100 * time.Millisecond):
	}
	releaseB()

	receiveWithin(t, shutdown, time.Second, "shutdown after every slot is released")
	if got, want := fake.recorded(), expectedActions(cache, []string{"save 0 a", "save 1 b"}); !slices.Equal(got, want) {
		t.Errorf("actions %q, want %q", got, want)
	}
}

func TestSlotCacheShutdownGivesUpWhenContextEnds(t *testing.T) {
	cache, fake := newTestSlotCache(t, 1, nil, nil, 0, runningProcess())
	release := holdSlot(t, cache, "a")
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	shutdown := make(chan struct{})
	go func() {
		cache.shutdown(ctx)
		close(shutdown)
	}()

	receiveWithin(t, shutdown, time.Second, "shutdown with an ended context")
	if got := fake.recorded(); got != nil {
		t.Errorf("actions %q, want none while a request holds the slot", got)
	}
}

func TestSlotCacheRejectsRequestsAfterShutdown(t *testing.T) {
	process := make(chan struct{})
	cache, _ := newTestSlotCache(t, 2, nil, nil, 0, process)
	cache.shutdown(context.Background())
	close(process)

	if _, _, err := cache.acquire(context.Background(), "a"); !errors.Is(err, errServerExited) {
		t.Errorf("error %v, want %v", err, errServerExited)
	}
}

func TestPinSlot(t *testing.T) {
	tests := []struct {
		name   string
		slotID int
		fields map[string]json.RawMessage
		want   map[string]string
	}{
		{
			name:   "empty body",
			slotID: 0,
			fields: map[string]json.RawMessage{},
			want:   map[string]string{"id_slot": "0", "cache_prompt": "true"},
		},
		{
			name:   "client slot settings are overridden and other fields kept",
			slotID: 2,
			fields: map[string]json.RawMessage{
				"model":        json.RawMessage(`"a"`),
				"id_slot":      json.RawMessage(`3`),
				"cache_prompt": json.RawMessage(`false`),
			},
			want: map[string]string{"model": `"a"`, "id_slot": "2", "cache_prompt": "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinSlot(tt.fields, tt.slotID)

			got := map[string]string{}
			for name, value := range tt.fields {
				got[name] = string(value)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("fields %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPrepareSlotDir(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr bool
	}{
		{
			name: "missing nested directory is created",
			path: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "cache", "slots")
			},
		},
		{
			name: "existing directory is kept",
			path: func(t *testing.T) string {
				return t.TempDir()
			},
		},
		{
			name: "path below a regular file fails",
			path: func(t *testing.T) string {
				file := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(file, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(file, "slots")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path(t)
			dir, err := prepareSlotDir(path)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "slot save path") {
					t.Fatalf("error %v, want a slot save path error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsAbs(dir) || dir != filepath.Clean(path) {
				t.Errorf("dir %q, want absolute %q", dir, path)
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Errorf("%q is not a directory: %v", dir, err)
			}
		})
	}
}

func TestWithSlotSavePath(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]any
		want  map[string]any
	}{
		{
			name:  "entry without flags",
			flags: nil,
			want:  map[string]any{slotSavePathFlag: "/slots"},
		},
		{
			name:  "model flags are kept",
			flags: map[string]any{"-ngl": 999},
			want:  map[string]any{"-ngl": 999, slotSavePathFlag: "/slots"},
		},
		{
			name:  "configured slot save path is replaced",
			flags: map[string]any{slotSavePathFlag: "/elsewhere"},
			want:  map[string]any{slotSavePathFlag: "/slots"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &config.LlamaCppEntry{Port: 12345, Flags: tt.flags}
			original := maps.Clone(tt.flags)

			got := withSlotSavePath(entry, "/slots")

			if !maps.Equal(got.Flags, tt.want) {
				t.Errorf("flags %v, want %v", got.Flags, tt.want)
			}
			if got.Port != entry.Port {
				t.Errorf("port %d, want %d", got.Port, entry.Port)
			}
			if !maps.Equal(entry.Flags, original) {
				t.Errorf("config flags changed to %v, want %v", entry.Flags, original)
			}
		})
	}
}

func TestSlotFileNamesModelAndSession(t *testing.T) {
	tests := []struct {
		selector string
		session  string
		want     string
	}{
		{"qwen3.6-35b", "simple", "qwen3.6-35b--a7a39b72f29718e6.bin"},
		{"qwen3.6-35b", "a/b", "qwen3.6-35b--c14cddc033f64b9d.bin"},
		{"qwen3.6-35b", "a:b", "qwen3.6-35b--6783a31eabf68ccc.bin"},
		{"org/model", "simple", "org--model--a7a39b72f29718e6.bin"},
	}

	for _, tt := range tests {
		t.Run(tt.selector+" "+tt.session, func(t *testing.T) {
			cache := &slotCache{selector: tt.selector}
			if got := cache.file(tt.session); got != tt.want {
				t.Errorf("file(%q) = %q, want %q", tt.session, got, tt.want)
			}
		})
	}
}

func TestSlotFileSeparatesModelsAndSessions(t *testing.T) {
	type slotOwner struct {
		selector string
		session  string
	}
	tests := []struct {
		name  string
		first slotOwner
		other slotOwner
	}{
		{"different models", slotOwner{"qwen3.6-35b", "session"}, slotOwner{"lfm2.5-24b", "session"}},
		{"same architecture models", slotOwner{"qwen3.6-35b", "session"}, slotOwner{"cyber-tiel", "session"}},
		{"selector and session split differently", slotOwner{"org/model", "simple"}, slotOwner{"org", "model/simple"}},
		{"sessions differing only in unsafe characters", slotOwner{"m", "a/b"}, slotOwner{"m", "a:b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := (&slotCache{selector: tt.first.selector}).file(tt.first.session)
			other := (&slotCache{selector: tt.other.selector}).file(tt.other.session)
			if first == other {
				t.Errorf("%+v and %+v share slot file %q", tt.first, tt.other, first)
			}
		})
	}
}

func TestSlotFileStaysPathSafe(t *testing.T) {
	for _, session := range []string{"a/b", "a\\b", "../escape", "with unicode ñ", ""} {
		name := (&slotCache{selector: "org/model"}).file(session)
		if strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, slotFileExtension) {
			t.Errorf("file(%q) = %q is not a plain %s file name", session, name, slotFileExtension)
		}
	}
}

func TestSanitizeFileNameStaysPathSafe(t *testing.T) {
	for _, id := range []string{"a/b", "a\\b", "../escape", "a b/c", "hijau:12"} {
		name := sanitizeFileName(id)
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("sanitizeFileName(%q) = %q still contains path separators", id, name)
		}
	}
}

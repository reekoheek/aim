package llamaserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const (
	cacheSlot             = 0
	slotSavePathFlag      = "--slot-save-path"
	slotFileExtension     = ".bin"
	slotFileSeparator     = "--"
	sessionDigestLength   = 16
	maxSlotSweepInterval  = time.Hour
	slotPruneReasonExpiry = "expired"
	slotPruneReasonLimit  = "over limit"
)

var errServerExited = errors.New("llama.cpp server has exited")

var slotClient = &http.Client{Timeout: time.Minute}

var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type slotCache struct {
	dir        string
	selector   string
	baseURL    string
	expiration time.Duration
	maxFiles   int
	logger     *logger.Logger
	done       <-chan struct{}
	slot       chan struct{}
	session    string
	unsaved    bool
}

func newSlotCache(dir string, selector string, baseURL string, server config.ServerConfig, l *logger.Logger, done <-chan struct{}) *slotCache {
	return &slotCache{
		dir:        dir,
		selector:   selector,
		baseURL:    baseURL,
		expiration: server.SlotExpiration,
		maxFiles:   server.SlotMaxFiles,
		logger:     l,
		done:       done,
		slot:       make(chan struct{}, 1),
	}
}

func prepareSlotDir(path string) (string, error) {
	dir, err := filepath.Abs(path)
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return "", fmt.Errorf("slot save path %q: %w", path, err)
	}
	return dir, nil
}

func withSlotSavePath(entry *config.LlamaCppEntry, dir string) *config.LlamaCppEntry {
	withSlots := *entry
	withSlots.Flags = make(map[string]any, len(entry.Flags)+1)
	maps.Copy(withSlots.Flags, entry.Flags)
	withSlots.Flags[slotSavePathFlag] = dir
	return &withSlots
}

func (c *slotCache) acquire(ctx context.Context, sessionID string) (release func(), err error) {
	start := time.Now()
	select {
	case c.slot <- struct{}{}:
	case <-c.done:
		return nil, errServerExited
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	acquired := time.Now()
	c.logger.Info("slot acquired",
		slog.Int("slot", cacheSlot),
		slog.String("session", sessionID),
		slog.Duration("waited", acquired.Sub(start)),
	)

	if c.session != sessionID {
		c.markUsed(sessionID)
		c.saveUnsaved()
		c.session = ""
		if sessionID != "" && c.restore(sessionID) {
			c.session = sessionID
		}
		c.prune()
	}

	return func() {
		defer func() { <-c.slot }()

		if sessionID != "" {
			c.session = sessionID
			c.unsaved = true
		}

		c.logger.Info("slot released",
			slog.Int("slot", cacheSlot),
			slog.String("session", sessionID),
			slog.Duration("held", time.Since(acquired)),
		)
	}, nil
}

func (c *slotCache) pin(fields map[string]json.RawMessage) {
	fields["id_slot"], _ = json.Marshal(cacheSlot)
	fields["cache_prompt"], _ = json.Marshal(true)
}

func (c *slotCache) shutdown(ctx context.Context) {
	select {
	case c.slot <- struct{}{}:
		if !c.processExited() {
			c.saveUnsaved()
			c.prune()
		}
	case <-c.done:
	case <-ctx.Done():
	}
}

func (c *slotCache) processExited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *slotCache) saveUnsaved() {
	if c.session == "" || !c.unsaved {
		return
	}
	file := c.file(c.session)
	c.unsaved = false
	if err := c.action("save", file); err != nil {
		c.logger.LogError("failed to save slot", err, slog.String("session", c.session), slog.String("file", file))
	}
}

func (c *slotCache) prune() {
	pruned, err := pruneSlots(c.dir, c.expiration, c.maxFiles, time.Now())
	logPrunedSlots(c.logger, c.dir, pruned, err)
}

func (c *slotCache) markUsed(sessionID string) {
	if sessionID == "" {
		return
	}
	file := c.file(sessionID)
	if err := touchSlotFile(filepath.Join(c.dir, file), time.Now()); err != nil {
		c.logger.LogError("failed to mark slot cache as used", err, slog.String("session", sessionID), slog.String("file", file))
	}
}

func (c *slotCache) restore(sessionID string) bool {
	file := c.file(sessionID)
	if _, err := os.Stat(filepath.Join(c.dir, file)); err != nil {
		return true
	}
	if err := c.action("restore", file); err != nil {
		c.logger.LogError("failed to restore slot", err, slog.String("session", sessionID), slog.String("file", file))
		return false
	}
	return true
}

func (c *slotCache) file(sessionID string) string {
	return sanitizeFileName(c.selector) + slotFileSeparator + sessionDigest(sessionID) + slotFileExtension
}

func sessionDigest(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])[:sessionDigestLength]
}

func sanitizeFileName(id string) string {
	return unsafeFileNameChars.ReplaceAllString(id, "--")
}

func (c *slotCache) action(action string, file string) error {
	start := time.Now()
	url := fmt.Sprintf("%s/slots/%d?action=%s", c.baseURL, cacheSlot, action)
	payload, err := json.Marshal(map[string]string{"filename": file})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	resp, err := slotClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s slot %d: %w", action, cacheSlot, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s slot %d: status %d: %s", action, cacheSlot, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var res struct {
		NSaved    int `json:"n_saved"`
		NRestored int `json:"n_restored"`
	}
	_ = json.Unmarshal(body, &res)

	c.logger.Info("slot cache", slog.String("action", action),
		slog.Int("slot", cacheSlot),
		slog.String("file", file),
		slog.Int("tokens", res.NSaved+res.NRestored),
		slog.Duration("took", time.Since(start)),
	)
	return nil
}

type prunedSlot struct {
	name   string
	reason string
}

type slotFileInfo struct {
	name     string
	modified time.Time
}

func PruneSlots(ctx context.Context, server config.ServerConfig, l *logger.Logger) {
	interval := maxSlotSweepInterval
	if server.SlotExpiration > 0 {
		interval = min(server.SlotExpiration, maxSlotSweepInterval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pruned, err := pruneSlots(server.SaveSlotPath, server.SlotExpiration, server.SlotMaxFiles, time.Now())
		logPrunedSlots(l, server.SaveSlotPath, pruned, err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func logPrunedSlots(l *logger.Logger, dir string, pruned []prunedSlot, err error) {
	for _, slot := range pruned {
		l.Info("slot cache removed", slog.String("file", slot.name), slog.String("reason", slot.reason))
	}
	if err != nil {
		l.LogError("failed to remove slot caches", err, slog.String("dir", dir))
	}
}

func pruneSlots(dir string, expiration time.Duration, maxFiles int, now time.Time) ([]prunedSlot, error) {
	files, err := listSlotFiles(dir)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(files, func(a slotFileInfo, b slotFileInfo) int {
		return b.modified.Compare(a.modified)
	})

	var pruned []prunedSlot
	var errs []error
	for i, file := range files {
		var reason string
		switch {
		case expiration > 0 && now.Sub(file.modified) > expiration:
			reason = slotPruneReasonExpiry
		case maxFiles > 0 && i >= maxFiles:
			reason = slotPruneReasonLimit
		default:
			continue
		}
		if err := os.Remove(filepath.Join(dir, file.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		pruned = append(pruned, prunedSlot{name: file.name, reason: reason})
	}
	return pruned, errors.Join(errs...)
}

func listSlotFiles(dir string) ([]slotFileInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var files []slotFileInfo
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != slotFileExtension {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, slotFileInfo{name: entry.Name(), modified: info.ModTime()})
	}
	return files, nil
}

func touchSlotFile(path string, now time.Time) error {
	if err := os.Chtimes(path, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

package llamacpp

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const (
	slotFileExtension     = ".bin"
	maxSlotSweepInterval  = time.Hour
	slotPruneReasonExpiry = "expired"
	slotPruneReasonLimit  = "over limit"
)

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

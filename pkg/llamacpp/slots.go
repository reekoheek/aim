package llamacpp

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

const (
	slotFileExtension    = ".bin"
	maxSlotSweepInterval = time.Hour
)

func ExpireSlots(ctx context.Context, dir string, expiration time.Duration, l *logger.Logger) {
	ticker := time.NewTicker(min(expiration, maxSlotSweepInterval))
	defer ticker.Stop()
	for {
		removed, err := removeExpiredSlots(dir, expiration, time.Now())
		for _, name := range removed {
			l.Info("slot cache expired", slog.String("file", name), slog.Duration("expiration", expiration))
		}
		if err != nil {
			l.LogError("failed to remove expired slot caches", err, slog.String("dir", dir))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func removeExpiredSlots(dir string, expiration time.Duration, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != slotFileExtension {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= expiration {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, entry.Name())
	}
	return removed, errors.Join(errs...)
}

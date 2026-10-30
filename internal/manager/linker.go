package manager

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/reekoheek/aim/pkg/backend"
)

type LinkReport struct {
	Linked  []string
	Removed []string
	Skipped []string
}

type Linker struct {
	Descriptor backend.Descriptor
	Root       string
	BinDir     string
}

func (l Linker) Relink(buildPath string) (LinkReport, error) {
	var report LinkReport
	if err := os.MkdirAll(l.BinDir, defaultDirectoryMode); err != nil {
		return report, err
	}
	wanted := l.wantedBinaries(buildPath)
	removed, err := l.removeStaleLinks(wanted)
	report.Removed = removed
	if err != nil {
		return report, err
	}
	for _, name := range l.Descriptor.LinkedBinaries {
		if !wanted[name] {
			continue
		}
		linked, err := l.link(name)
		if err != nil {
			return report, err
		}
		if linked {
			report.Linked = append(report.Linked, name)
		} else {
			report.Skipped = append(report.Skipped, name)
		}
	}
	return report, nil
}

func (l Linker) wantedBinaries(buildPath string) map[string]bool {
	wanted := map[string]bool{}
	for _, name := range l.Descriptor.LinkedBinaries {
		info, err := os.Stat(filepath.Join(buildPath, name))
		if err == nil && info.Mode().IsRegular() && info.Mode()&executableBits != 0 {
			wanted[name] = true
		}
	}
	return wanted
}

func (l Linker) removeStaleLinks(wanted map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(l.BinDir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if wanted[name] || entry.Type()&fs.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(l.BinDir, name)
		if !l.pointsIntoRoot(path) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
}

func (l Linker) pointsIntoRoot(linkPath string) bool {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}
	return isWithin(l.Root, filepath.Clean(target))
}

func (l Linker) link(name string) (bool, error) {
	path := filepath.Join(l.BinDir, name)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	case info.Mode()&fs.ModeSymlink == 0:
		return false, nil
	default:
		if err := os.Remove(path); err != nil {
			return false, err
		}
	}
	return true, os.Symlink(filepath.Join(l.Root, CurrentName, name), path)
}

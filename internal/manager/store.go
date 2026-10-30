package manager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/reekoheek/aim/pkg/backend"
)

const (
	CurrentName         = "current"
	stagingPrefix       = ".staging-"
	pendingCurrentName  = CurrentName + ".new"
	executableBits      = 0o111
	unknownBuildNumber  = -1
	currentMarkerMode   = 0o644
	listIndexMaxDigits  = 2
	ambiguousBuildIndex = "%4d  %s"
)

var listIndexPattern = regexp.MustCompile(fmt.Sprintf(`^\d{1,%d}$`, listIndexMaxDigits))

type Build struct {
	Index   int
	Folder  string
	Path    string
	Build   string
	Variant string
	Stable  string
	Managed bool
	Number  int
	ModTime time.Time
}

type Store struct {
	Descriptor  backend.Descriptor
	Root        string
	Platform    backend.Platform
	UseSymlinks bool
}

type AmbiguousBuildError struct {
	Argument string
	Matches  []Build
}

func (e *AmbiguousBuildError) Error() string {
	lines := []string{fmt.Sprintf("%q matches several builds; pick one by index or folder name:", e.Argument)}
	for _, build := range e.Matches {
		lines = append(lines, fmt.Sprintf(ambiguousBuildIndex, build.Index, build.Folder))
	}
	return strings.Join(lines, "\n")
}

var ErrNoMatchingBuild = errors.New("no installed build matches")

func (s Store) MainBinaryPath(folder string) string {
	return filepath.Join(s.Root, folder, backend.ExecutableName(s.Descriptor.MainBinary, s.Platform.OS))
}

func (s Store) BuildPath(folder string) string {
	return filepath.Join(s.Root, folder)
}

func (s Store) Builds() ([]Build, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var builds []Build
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == CurrentName || entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		if !s.hasMainBinary(name) {
			continue
		}
		builds = append(builds, s.describe(name))
	}
	sort.SliceStable(builds, func(left int, right int) bool {
		if builds[left].Number != builds[right].Number {
			return builds[left].Number > builds[right].Number
		}
		return builds[left].ModTime.After(builds[right].ModTime)
	})
	for index := range builds {
		builds[index].Index = index + 1
	}
	return builds, nil
}

func (s Store) hasMainBinary(folder string) bool {
	info, err := os.Stat(s.MainBinaryPath(folder))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return s.Platform.OS == "windows" || info.Mode()&executableBits != 0
}

func (s Store) describe(folder string) Build {
	path := s.BuildPath(folder)
	build := Build{Folder: folder, Path: path, Number: unknownBuildNumber}
	if info, err := os.Stat(path); err == nil {
		build.ModTime = info.ModTime()
	}
	stamp := ReadStamp(path)
	build.Stable = stamp.Stable
	if tag, variant, ok := s.Descriptor.ParseFolderName(folder); ok {
		build.Build = tag
		build.Variant = variant
		build.Managed = true
		if number, ok := s.Descriptor.BuildNumber(tag); ok {
			build.Number = number
		}
	}
	return build
}

func (s Store) Current() (string, error) {
	path := filepath.Join(s.Root, CurrentName)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return filepath.Base(filepath.Clean(target)), nil
	}
	if info.Mode().IsRegular() {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(content)), nil
	}
	return "", fmt.Errorf("%s is a real directory, not a link; move it aside first", path)
}

func (s Store) Activate(folder string) error {
	if _, err := s.Current(); err != nil {
		return err
	}
	pending := filepath.Join(s.Root, pendingCurrentName)
	if err := os.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if s.UseSymlinks {
		if err := os.Symlink(folder, pending); err != nil {
			return err
		}
	} else if err := os.WriteFile(pending, []byte(folder+"\n"), currentMarkerMode); err != nil {
		return err
	}
	return os.Rename(pending, filepath.Join(s.Root, CurrentName))
}

func (s Store) Find(argument string, builds []Build) (Build, error) {
	for _, build := range builds {
		if build.Folder == argument {
			return build, nil
		}
	}
	if listIndexPattern.MatchString(argument) {
		index, _ := strconv.Atoi(argument)
		if index < 1 || index > len(builds) {
			return Build{}, fmt.Errorf("index %d out of range (1-%d)", index, len(builds))
		}
		return builds[index-1], nil
	}
	matches := s.matchVersion(argument, builds)
	if len(matches) == 0 {
		matches = matchSubstring(argument, builds)
	}
	switch len(matches) {
	case 0:
		return Build{}, fmt.Errorf("%w %q", ErrNoMatchingBuild, argument)
	case 1:
		return matches[0], nil
	default:
		return Build{}, &AmbiguousBuildError{Argument: argument, Matches: matches}
	}
}

func (s Store) matchVersion(argument string, builds []Build) []Build {
	version, err := s.Descriptor.ParseVersion(argument)
	if err != nil {
		return nil
	}
	var matches []Build
	for _, build := range builds {
		switch version.Kind {
		case backend.VersionStable:
			if build.Stable == version.Tag {
				matches = append(matches, build)
			}
		case backend.VersionBuild:
			if build.Build != "" && (build.Build == version.Tag || (version.Tag == "" && build.Number == version.Number)) {
				matches = append(matches, build)
			}
		}
	}
	return matches
}

func matchSubstring(argument string, builds []Build) []Build {
	var matches []Build
	for _, build := range builds {
		if strings.Contains(build.Folder, argument) {
			matches = append(matches, build)
		}
	}
	return matches
}

func (s Store) Remove(build Build) error {
	return os.RemoveAll(build.Path)
}

func (s Store) Prune(variant string, keep int, protected string) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	builds, err := s.Builds()
	if err != nil {
		return nil, err
	}
	current, err := s.Current()
	if err != nil {
		return nil, err
	}
	var pruned []string
	kept := 0
	for _, build := range builds {
		if !build.Managed || build.Variant != variant {
			continue
		}
		kept++
		if kept <= keep || build.Folder == current || build.Folder == protected {
			continue
		}
		if err := s.Remove(build); err != nil {
			return pruned, err
		}
		pruned = append(pruned, build.Folder)
	}
	return pruned, nil
}

func (s Store) NewStaging() (string, error) {
	if err := os.MkdirAll(s.Root, defaultDirectoryMode); err != nil {
		return "", err
	}
	return os.MkdirTemp(s.Root, stagingPrefix)
}

func DirectorySize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

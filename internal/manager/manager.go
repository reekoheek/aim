package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reekoheek/aim/pkg/backend"
)

const (
	DefaultKeep         = 3
	DefaultListLimit    = 10
	AllListLimit        = 40
	stableListLimit     = 5
	suggestionLimit     = 6
	archiveFileName     = "archive"
	extractFolderName   = "build"
	publishedDateLayout = "2006-01-02"
)

type Manager struct {
	Descriptor  backend.Descriptor
	Store       Store
	Linker      Linker
	Resolver    Resolver
	Platform    backend.Platform
	Output      io.Writer
	Input       io.Reader
	Interactive bool
}

func New(descriptor backend.Descriptor, source ReleaseSource, platform backend.Platform, root string, binDir string) *Manager {
	return &Manager{
		Descriptor: descriptor,
		Store:      Store{Descriptor: descriptor, Root: root, Platform: platform, UseSymlinks: platform.OS != "windows"},
		Linker:     Linker{Descriptor: descriptor, Root: root, BinDir: binDir},
		Resolver:   Resolver{Descriptor: descriptor, Source: source},
		Platform:   platform,
		Output:     io.Discard,
		Input:      strings.NewReader(""),
	}
}

func (m *Manager) printf(format string, arguments ...any) {
	fmt.Fprintf(m.Output, format, arguments...)
}

type InstallOptions struct {
	Version string
	Variant string
	Force   bool
	DryRun  bool
	NoUse   bool
	Keep    int
}

func (m *Manager) Install(ctx context.Context, options InstallOptions) error {
	version, err := m.Descriptor.ParseVersion(options.Version)
	if err != nil {
		return err
	}
	variant, err := m.installVariant(options.Variant)
	if err != nil {
		return err
	}
	resolution, err := m.Resolver.Resolve(ctx, version)
	if err != nil {
		return err
	}
	asset, ok := m.Descriptor.PickAsset(resolution.Release, m.Platform, variant)
	if !ok {
		return m.missingAssetError(ctx, resolution.Build, variant)
	}
	folder := m.Descriptor.FolderName(resolution.Build, variant)
	m.printf(">> %s (%s) %s\n", describeResolution(resolution), variant, asset.Name)
	if options.DryRun {
		m.printf(">> dry-run: would install %s from\n   %s\n", folder, asset.URL)
		return nil
	}

	current, err := m.Store.Current()
	if err != nil {
		return err
	}
	installed := m.Store.hasMainBinary(folder)
	switch {
	case installed && !options.Force && folder == current && !options.NoUse:
		m.printf(">> already on %s; use -f to re-download\n", folder)
		return nil
	case installed && !options.Force:
		m.printf(">> %s already downloaded\n", folder)
	default:
		if err := m.download(ctx, asset, resolution.Build, folder); err != nil {
			return err
		}
	}
	if err := WriteStamp(m.Store.BuildPath(folder), Stamp{Build: resolution.Build, Variant: variant, Stable: resolution.Stable}); err != nil {
		return err
	}

	if options.NoUse {
		m.printf(">> downloaded to %s (not activated)\n", folder)
	} else if err := m.activate(folder); err != nil {
		return err
	}
	pruned, err := m.Store.Prune(variant, options.Keep, folder)
	for _, name := range pruned {
		m.printf(">> pruned %s\n", name)
	}
	if err != nil {
		return err
	}
	if !options.NoUse {
		m.printRestartHint()
	}
	return nil
}

func describeResolution(resolution Resolution) string {
	if resolution.Stable != "" {
		return resolution.Stable + " -> " + resolution.Build
	}
	return resolution.Build
}

func (m *Manager) installVariant(requested string) (string, error) {
	if requested != "" {
		if !m.Descriptor.HasVariant(requested) {
			return "", fmt.Errorf("unknown %s variant %q (known: %s)", m.Descriptor.Name, requested, strings.Join(m.Descriptor.Variants, ", "))
		}
		return requested, nil
	}
	current, err := m.Store.Current()
	if err != nil {
		return "", err
	}
	if current != "" {
		if _, variant, ok := m.Descriptor.ParseFolderName(current); ok {
			return variant, nil
		}
		if stamp := ReadStamp(m.Store.BuildPath(current)); m.Descriptor.HasVariant(stamp.Variant) {
			return stamp.Variant, nil
		}
	}
	return m.Descriptor.DefaultVariantFor(m.Platform), nil
}

func (m *Manager) missingAssetError(ctx context.Context, build string, variant string) error {
	message := fmt.Sprintf("release %s has no %s/%s %s asset", build, m.Platform.OS, m.Platform.Arch, variant)
	alternatives, err := m.Resolver.BuildsWithAsset(ctx, m.Platform, variant, suggestionLimit)
	if err != nil || len(alternatives) == 0 {
		return errors.New(message)
	}
	return fmt.Errorf("%s\nnewest builds that do: %s\ne.g. aim backend %s add %s --variant %s",
		message, strings.Join(alternatives, ", "), m.Descriptor.Name, alternatives[0], variant)
}

func (m *Manager) download(ctx context.Context, asset backend.Asset, build string, folder string) error {
	staging, err := m.Store.NewStaging()
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	archivePath := filepath.Join(staging, archiveFileName)
	m.printf(">> downloading %s (%s)\n", asset.Name, HumanSize(asset.Size))
	if err := m.downloadTo(ctx, asset.URL, archivePath); err != nil {
		return err
	}
	m.printf(">> extracting\n")
	extracted := filepath.Join(staging, extractFolderName)
	if err := Extract(archivePath, asset.Format, m.Descriptor.StripPrefixFor(build), extracted); err != nil {
		return fmt.Errorf("extract %s: %w", asset.Name, err)
	}
	mainBinary := backend.ExecutableName(m.Descriptor.MainBinary, m.Platform.OS)
	if info, err := os.Stat(filepath.Join(extracted, mainBinary)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("archive %s contains no %s", asset.Name, mainBinary)
	}
	destination := m.Store.BuildPath(folder)
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return os.Rename(extracted, destination)
}

func (m *Manager) downloadTo(ctx context.Context, address string, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := m.Resolver.Source.Download(ctx, address, file); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (m *Manager) activate(folder string) error {
	if err := m.Store.Activate(folder); err != nil {
		return err
	}
	m.printf(">> current -> %s\n", folder)
	if !m.Store.UseSymlinks {
		m.printf(">> %s: %s\n", m.Descriptor.MainBinary, m.Store.MainBinaryPath(folder))
		return nil
	}
	report, err := m.Linker.Relink(m.Store.BuildPath(folder))
	for _, name := range report.Skipped {
		m.printf(">> skipped %s: %s is a real file\n", name, filepath.Join(m.Linker.BinDir, name))
	}
	if err != nil {
		return err
	}
	if len(report.Linked) > 0 {
		m.printf(">> linked %d binaries into %s\n", len(report.Linked), m.Linker.BinDir)
	}
	return nil
}

func (m *Manager) printRestartHint() {
	m.printf(">> no restart needed: aim serve picks up the new %s the next time it starts the backend\n", m.Descriptor.MainBinary)
}

type ListOptions struct {
	Limit int
	Local bool
}

func (m *Manager) List(ctx context.Context, options ListOptions) error {
	builds, err := m.Store.Builds()
	if err != nil {
		return err
	}
	current, err := m.Store.Current()
	if err != nil {
		return err
	}
	m.printInstalled(builds, current)
	if options.Local {
		return nil
	}
	return m.printAvailable(ctx, builds, options.Limit)
}

func (m *Manager) printInstalled(builds []Build, current string) {
	m.printf("Installed  (%s)\n", m.Store.Root)
	if len(builds) == 0 {
		m.printf("  (none)\n")
	}
	for _, build := range builds {
		mark := " "
		var tags []string
		if build.Folder == current {
			mark = "*"
			tags = append(tags, "active")
		}
		if build.Stable != "" {
			tags = append(tags, build.Stable)
		}
		if !build.Managed {
			tags = append(tags, "manual")
		}
		variant := build.Variant
		if variant == "" {
			variant = "-"
		}
		m.printf("  %s %2d  %-36s %-7s %7s %s\n", mark, build.Index, build.Folder, variant, HumanSize(DirectorySize(build.Path)), strings.Join(tags, " "))
	}
}

func (m *Manager) printAvailable(ctx context.Context, builds []Build, limit int) error {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	releases, err := m.Resolver.Source.Releases(ctx, m.Descriptor.Repository, releasePageSize, 1)
	if err != nil {
		return err
	}
	installed := map[string][]string{}
	for _, build := range builds {
		if build.Managed {
			installed[build.Build] = append(installed[build.Build], build.Variant)
		}
	}
	m.printf("\nAvailable  (github.com/%s)\n", m.Descriptor.Repository)
	if m.Descriptor.SupportsStable() {
		if err := m.printStable(ctx, releases, installed); err != nil {
			return err
		}
		m.printf("  build\n")
	}
	shown := 0
	for _, release := range releases {
		if shown >= limit {
			break
		}
		if !m.Descriptor.IsBuildTag(release.Tag) {
			continue
		}
		shown++
		m.printf("    %-22s %s  %s\n", release.Tag, release.PublishedAt.Format(publishedDateLayout), installedMarker(installed[release.Tag]))
	}
	return nil
}

func (m *Manager) printStable(ctx context.Context, releases []backend.Release, installed map[string][]string) error {
	m.printf("  stable\n")
	shown := 0
	for _, release := range releases {
		if shown >= stableListLimit {
			break
		}
		if !m.Descriptor.IsStableTag(release.Tag) {
			continue
		}
		shown++
		build, err := m.Resolver.StableBuild(ctx, release)
		if err != nil {
			return err
		}
		if build == "" {
			build = "?"
		}
		m.printf("    %-9s -> %-10s %s  %s\n", release.Tag, build, release.PublishedAt.Format(publishedDateLayout), installedMarker(installed[build]))
	}
	return nil
}

func installedMarker(variants []string) string {
	if len(variants) == 0 {
		return ""
	}
	return "installed: " + strings.Join(variants, ",")
}

func (m *Manager) Use(argument string) error {
	builds, err := m.Store.Builds()
	if err != nil {
		return err
	}
	if len(builds) == 0 {
		return fmt.Errorf("no builds in %s; run: aim backend %s add", m.Store.Root, m.Descriptor.Name)
	}
	current, err := m.Store.Current()
	if err != nil {
		return err
	}
	if argument == "" {
		argument, err = m.pick(builds, current)
		if err != nil || argument == "" {
			return err
		}
	}
	target, err := m.Store.Find(argument, builds)
	if err != nil {
		return m.withInstallHint(err, argument)
	}
	if target.Folder == current {
		m.printf(">> %s is already current\n", target.Folder)
		return nil
	}
	if err := m.activate(target.Folder); err != nil {
		return err
	}
	m.printRestartHint()
	return nil
}

func (m *Manager) withInstallHint(err error, argument string) error {
	if !errors.Is(err, ErrNoMatchingBuild) {
		return err
	}
	if version, parseErr := m.Descriptor.ParseVersion(argument); parseErr == nil && (version.Kind == backend.VersionBuild || version.Kind == backend.VersionStable) {
		return fmt.Errorf("%w\nnot installed; try: aim backend %s add %s", err, m.Descriptor.Name, argument)
	}
	return err
}

func (m *Manager) pick(builds []Build, current string) (string, error) {
	for _, build := range builds {
		mark := " "
		suffix := ""
		if build.Folder == current {
			mark = "*"
			suffix = " (current)"
		}
		m.printf("  %s %2d  %s%s\n", mark, build.Index, build.Folder, suffix)
	}
	m.printf("\npick a number (Enter = cancel): ")
	answer := m.readLine()
	if answer == "" {
		m.printf("cancelled\n")
	}
	return answer, nil
}

func (m *Manager) readLine() string {
	line, _ := bufio.NewReader(m.Input).ReadString('\n')
	return strings.TrimSpace(line)
}

func (m *Manager) Remove(arguments []string, confirmed bool) error {
	if len(arguments) == 0 {
		return fmt.Errorf("rm needs a version, index or folder name (see: aim backend %s ls)", m.Descriptor.Name)
	}
	builds, err := m.Store.Builds()
	if err != nil {
		return err
	}
	current, err := m.Store.Current()
	if err != nil {
		return err
	}
	var targets []Build
	seen := map[string]bool{}
	for _, argument := range arguments {
		target, err := m.Store.Find(argument, builds)
		if err != nil {
			return err
		}
		if target.Folder == current {
			return fmt.Errorf("refusing to delete the active build %s; switch away first: aim backend %s use <other>", target.Folder, m.Descriptor.Name)
		}
		if !seen[target.Folder] {
			seen[target.Folder] = true
			targets = append(targets, target)
		}
	}

	m.printf("about to delete:\n")
	for _, target := range targets {
		m.printf("  %s  %s\n", target.Folder, HumanSize(DirectorySize(target.Path)))
	}
	if !confirmed {
		if !m.Interactive {
			return errors.New("not a terminal; pass -y to confirm")
		}
		m.printf("delete %d build(s)? [y/N] ", len(targets))
		if answer := strings.ToLower(m.readLine()); answer != "y" && answer != "yes" {
			m.printf("cancelled\n")
			return nil
		}
	}
	for _, target := range targets {
		m.printf(">> deleting %s\n", target.Folder)
		if err := m.Store.Remove(target); err != nil {
			return err
		}
	}
	return nil
}

func HumanSize(bytes int64) string {
	const unit = 1024
	units := []string{"B", "K", "M", "G", "T"}
	size := float64(bytes)
	index := 0
	for size >= unit && index < len(units)-1 {
		size /= unit
		index++
	}
	if index < 2 {
		return fmt.Sprintf("%.0f%s", size, units[index])
	}
	return fmt.Sprintf("%.1f%s", size, units[index])
}

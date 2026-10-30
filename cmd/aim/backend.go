package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reekoheek/aim/internal/manager"
	"github.com/reekoheek/aim/pkg/backend"
)

type backendDependencies struct {
	source      manager.ReleaseSource
	environment manager.Environment
	platform    backend.Platform
	input       io.Reader
	interactive bool
}

func defaultBackendDependencies() backendDependencies {
	home, _ := os.UserHomeDir()
	return backendDependencies{
		source:      manager.NewGitHub(manager.TokenFromEnvironment(os.Getenv)),
		environment: manager.Environment{OS: runtime.GOOS, Home: home, Lookup: os.Getenv},
		platform:    backend.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		input:       os.Stdin,
		interactive: isTerminal(os.Stdin),
	}
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func showHelp(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

type backendPaths struct {
	root   string
	binDir string
}

func newBackendCmd(dependencies backendDependencies) *cobra.Command {
	var paths backendPaths

	cmd := &cobra.Command{
		Use:   "backend",
		Short: "Add, list, switch and remove prebuilt backends (" + strings.Join(backend.Names(), ", ") + ")",
		Args:  cobra.NoArgs,
		RunE:  showHelp,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.SilenceUsage = true
		},
	}
	cmd.PersistentFlags().StringVar(&paths.root, "root", "", "backend root folder (default: <user data dir>/<backend>, env AIM_<BACKEND>_ROOT)")
	cmd.PersistentFlags().StringVar(&paths.binDir, "bin-dir", "", "folder for binary links (default: ~/.local/bin, env "+manager.BinDirEnv+")")

	for _, name := range backend.Names() {
		descriptor, _ := backend.Lookup(name)
		cmd.AddCommand(newBackendDescriptorCmd(descriptor, dependencies, &paths))
	}

	return cmd
}

func newBackendDescriptorCmd(descriptor backend.Descriptor, dependencies backendDependencies, paths *backendPaths) *cobra.Command {
	cmd := &cobra.Command{
		Use:   descriptor.Name,
		Short: "Manage " + descriptor.Name + " prebuilt releases from github.com/" + descriptor.Repository,
		Args:  cobra.NoArgs,
		RunE:  showHelp,
	}

	newManager := func(command *cobra.Command) (*manager.Manager, error) {
		root, err := dependencies.environment.Root(descriptor.Name, paths.root)
		if err != nil {
			return nil, fmt.Errorf("resolve backend root: %w", err)
		}
		binDir, err := dependencies.environment.BinDir(paths.binDir)
		if err != nil {
			return nil, fmt.Errorf("resolve bin dir: %w", err)
		}
		backendManager := manager.New(descriptor, dependencies.source, dependencies.platform, root, binDir)
		backendManager.Output = command.OutOrStdout()
		backendManager.Input = dependencies.input
		backendManager.Interactive = dependencies.interactive
		return backendManager, nil
	}

	cmd.AddCommand(
		newBackendListCmd(newManager),
		newBackendInstallCmd(descriptor, newManager),
		newBackendUseCmd(newManager),
		newBackendRemoveCmd(newManager),
	)

	return cmd
}

type managerFactory func(command *cobra.Command) (*manager.Manager, error)

func newBackendListCmd(newManager managerFactory) *cobra.Command {
	options := manager.ListOptions{Limit: manager.DefaultListLimit}
	var all bool

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List installed builds and available upstream releases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			backendManager, err := newManager(cmd)
			if err != nil {
				return err
			}
			if all {
				options.Limit = manager.AllListLimit
			}
			return backendManager.List(cmd.Context(), options)
		},
	}
	cmd.Flags().IntVarP(&options.Limit, "limit", "n", manager.DefaultListLimit, "number of upstream builds to show")
	cmd.Flags().BoolVarP(&all, "all", "a", false, fmt.Sprintf("show %d upstream builds", manager.AllListLimit))
	cmd.Flags().BoolVarP(&options.Local, "local", "l", false, "installed builds only, no network")

	return cmd
}

func newBackendInstallCmd(descriptor backend.Descriptor, newManager managerFactory) *cobra.Command {
	options := manager.InstallOptions{Keep: manager.DefaultKeep}

	cmd := &cobra.Command{
		Use:   "add [VERSION]",
		Short: "Download a release and switch to it",
		Long:  installLongHelp(descriptor),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			backendManager, err := newManager(cmd)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				options.Version = args[0]
			}
			return backendManager.Install(cmd.Context(), options)
		},
	}
	cmd.Flags().StringVar(&options.Variant, "variant", "", "prebuilt variant: "+strings.Join(descriptor.Variants, ", ")+" (default: variant of the active build, else "+descriptor.DefaultVariant+")")
	cmd.Flags().BoolVarP(&options.Force, "force", "f", false, "re-download even if already installed")
	cmd.Flags().BoolVar(&options.DryRun, "dry-run", false, "print the resolved asset URL only")
	cmd.Flags().BoolVar(&options.NoUse, "no-use", false, "download only, do not switch current")
	cmd.Flags().IntVar(&options.Keep, "keep", manager.DefaultKeep, "builds of the same variant to keep when pruning (0 = never prune)")

	return cmd
}

func installLongHelp(descriptor backend.Descriptor) string {
	lines := []string{"Download a " + descriptor.Name + " release and switch to it.", "", "VERSION:"}
	if descriptor.SupportsStable() {
		lines = append(lines,
			"  latest | stable   newest stable release (default)",
			"  dev | nightly     newest build",
			"  vX.Y.Z            that stable release, resolved to its build",
			"  bNNNNN | NNNNN    that build",
		)
	} else {
		lines = append(lines,
			"  latest | dev      newest build (default)",
			"  master-NNNN-<sha> that build",
			"  NNNN              the build with that number",
		)
	}
	return strings.Join(lines, "\n")
}

func newBackendUseCmd(newManager managerFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "use [VERSION|INDEX|FOLDER]",
		Short: "Switch to an installed build (menu without argument)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			backendManager, err := newManager(cmd)
			if err != nil {
				return err
			}
			argument := ""
			if len(args) == 1 {
				argument = args[0]
			}
			return backendManager.Use(argument)
		},
	}
}

func newBackendRemoveCmd(newManager managerFactory) *cobra.Command {
	var confirmed bool

	cmd := &cobra.Command{
		Use:   "rm VERSION|INDEX|FOLDER...",
		Short: "Delete installed builds (never the active one)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			backendManager, err := newManager(cmd)
			if err != nil {
				return err
			}
			return backendManager.Remove(args, confirmed)
		},
	}
	cmd.Flags().BoolVarP(&confirmed, "yes", "y", false, "skip confirmation")

	return cmd
}

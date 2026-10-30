package manager

import (
	"errors"
	"path/filepath"
	"strings"
)

const (
	rootEnvPrefix = "AIM_"
	rootEnvSuffix = "_ROOT"
	BinDirEnv     = "AIM_BIN_DIR"
)

var errNoHomeDirectory = errors.New("cannot determine the home directory")

type Environment struct {
	OS     string
	Home   string
	Lookup func(key string) string
}

func (e Environment) UserDataDir() (string, error) {
	switch e.OS {
	case "windows":
		if directory := e.Lookup("LocalAppData"); directory != "" {
			return directory, nil
		}
		return "", errors.New("%LocalAppData% is not set")
	case "darwin":
		if e.Home == "" {
			return "", errNoHomeDirectory
		}
		return filepath.Join(e.Home, "Library", "Application Support"), nil
	default:
		if directory := e.Lookup("XDG_DATA_HOME"); filepath.IsAbs(directory) {
			return directory, nil
		}
		if e.Home == "" {
			return "", errNoHomeDirectory
		}
		return filepath.Join(e.Home, ".local", "share"), nil
	}
}

func RootEnvName(backendName string) string {
	normalized := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' {
			return character - 'a' + 'A'
		}
		if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			return character
		}
		return '_'
	}, backendName)
	return rootEnvPrefix + normalized + rootEnvSuffix
}

func (e Environment) Root(backendName string, override string) (string, error) {
	if override == "" {
		override = e.Lookup(RootEnvName(backendName))
	}
	if override != "" {
		return filepath.Abs(override)
	}
	dataDir, err := e.UserDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, backendName), nil
}

func (e Environment) BinDir(override string) (string, error) {
	if override == "" {
		override = e.Lookup(BinDirEnv)
	}
	if override != "" {
		return filepath.Abs(override)
	}
	if e.Home == "" {
		return "", errNoHomeDirectory
	}
	return filepath.Join(e.Home, ".local", "bin"), nil
}

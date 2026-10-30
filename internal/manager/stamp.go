package manager

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	StampFileName       = ".aim-stamp"
	legacyStampFileName = ".llama-manager"
)

type Stamp struct {
	Build   string
	Variant string
	Stable  string
}

func WriteStamp(directory string, stamp Stamp) error {
	var content strings.Builder
	fmt.Fprintf(&content, "build=%s\nvariant=%s\n", stamp.Build, stamp.Variant)
	if stamp.Stable != "" {
		fmt.Fprintf(&content, "stable=%s\n", stamp.Stable)
	}
	return os.WriteFile(filepath.Join(directory, StampFileName), []byte(content.String()), 0o644)
}

func ReadStamp(directory string) Stamp {
	for _, name := range []string{StampFileName, legacyStampFileName} {
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		defer file.Close()
		return parseStamp(bufio.NewScanner(file))
	}
	return Stamp{}
}

func parseStamp(scanner *bufio.Scanner) Stamp {
	var stamp Stamp
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "build":
			stamp.Build = value
		case "variant":
			stamp.Variant = value
		case "stable":
			stamp.Stable = value
		}
	}
	return stamp
}

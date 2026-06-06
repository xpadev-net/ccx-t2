package ledger

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath checks that a path is a valid relative path within the repository.
// It must not be absolute and must not escape via "../".
func ValidatePath(p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("path must be relative: %s", p)
	}
	// Clean the path and check it doesn't escape.
	cleaned := filepath.Clean(p)
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("path escapes repository root: %s", p)
	}
	return nil
}

// NormalizePath removes trailing slashes for consistent comparison.
func NormalizePath(p string) string {
	return strings.TrimRight(p, "/")
}

// PathMatches reports whether path f matches pattern p using directory-boundary
// prefix matching (case-sensitive).
// Pattern p matches f if f == p or f starts with p + "/".
func PathMatches(p, f string) bool {
	p = NormalizePath(p)
	f = NormalizePath(f)
	if p == f {
		return true
	}
	return strings.HasPrefix(f, p+"/")
}

// ValidatePaths validates a slice of paths.
func ValidatePaths(paths []string) error {
	for _, p := range paths {
		if err := ValidatePath(p); err != nil {
			return err
		}
	}
	return nil
}

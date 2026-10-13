package featuredocs

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Assets contains short feature-use guides shipped with the binary.
// Development notes remain in the source repository and are not embedded.
//
//go:embed assets/*.md
var Assets embed.FS

// Files returns the bundled feature guide names in stable order.
func Files() ([]string, error) {
	var names []string
	err := fs.WalkDir(Assets, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && path != "." {
			names = append(names, path)
		}
		return nil
	})
	sort.Strings(names)
	return names, err
}

// DefaultDir returns the feature-guide directory beside an installed runtime.
func DefaultDir() string {
	if override := os.Getenv("CERAA_FEATURE_DOCS_DIR"); override != "" {
		return override
	}
	if exe, err := os.Executable(); err == nil {
		bin := filepath.Dir(exe)
		if filepath.Base(bin) == "bin" {
			return filepath.Join(filepath.Dir(bin), "docs")
		}
	}
	return filepath.Join("docs")
}

// Install writes the binary-authoritative feature guides to the runtime docs
// directory so the system prompt's navigation path exists for every run.
func Install(dir string) error {
	if dir == "" {
		return fmt.Errorf("feature docs directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create feature docs directory: %w", err)
	}
	entries, err := Files()
	if err != nil {
		return err
	}
	for _, name := range entries {
		data, err := Assets.ReadFile(name)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

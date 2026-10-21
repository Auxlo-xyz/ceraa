package nuclei

// load.go reads templates off disk and out of the binary.
//
// A template that fails to parse is collected as an error and the load goes
// on: one broken file in a corpus of thousands must not cost the other
// nine hundred and ninety nine. The errors travel with the templates, so
// the run can say what it skipped and why.
//
// The embedded set is always available. It is small and it is the checks
// that matter most, so a machine with no nuclei corpus installed still runs
// something useful.

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed templates/*.yaml
var embeddedTemplates embed.FS

// Load reads templates from a directory (recursively, .yaml and .yml) or a
// single file.
//
// The errors are the templates that could not be read. They are returned
// beside the templates, not instead of them.
func Load(path string) ([]Template, []error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, []error{fmt.Errorf("no template path was given")}
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return nil, []error{fmt.Errorf("the template path %s could not be read: %w", trimmed, err)}
	}
	if !info.IsDir() {
		tpl, err := LoadFile(trimmed)
		if err != nil {
			return nil, []error{err}
		}
		return []Template{tpl}, nil
	}

	var (
		templates []Template
		errs      []error
	)
	walkErr := filepath.WalkDir(trimmed, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("the template %s could not be read: %w", current, err))
			return nil
		}
		if entry.IsDir() || !isTemplateFile(current) {
			return nil
		}
		tpl, err := LoadFile(current)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		templates = append(templates, tpl)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].Path < templates[j].Path
	})
	return templates, errs
}

// LoadFile reads one template file.
func LoadFile(path string) (Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, fmt.Errorf("the template %s could not be read: %w", path, err)
	}
	tpl, err := Parse(data, path)
	if err != nil {
		return Template{}, err
	}
	return tpl, nil
}

// LoadEmbedded reads the templates shipped with the harness.
//
// The path of an embedded template is "embedded:<name>", so a finding can
// say where it came from without pretending it came from a file.
func LoadEmbedded() ([]Template, []error) {
	entries, err := fs.ReadDir(embeddedTemplates, "templates")
	if err != nil {
		return nil, []error{fmt.Errorf("the embedded templates could not be read: %w", err)}
	}
	var (
		templates []Template
		errs      []error
	)
	for _, entry := range entries {
		if entry.IsDir() || !isTemplateFile(entry.Name()) {
			continue
		}
		name := "templates/" + entry.Name()
		data, err := embeddedTemplates.ReadFile(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("the embedded template %s could not be read: %w", name, err))
			continue
		}
		tpl, err := Parse(data, "embedded:"+entry.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		templates = append(templates, tpl)
	}
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].ID < templates[j].ID
	})
	return templates, errs
}

// LoadDefault reads the default directories and the embedded set.
//
// The directories come first and the embedded set last, so an installed
// corpus wins over the shipped one on a template id.
func LoadDefault() ([]Template, []error) {
	var (
		templates []Template
		errs      []error
	)
	for _, dir := range DefaultTemplateDirs() {
		loaded, loadErrs := Load(dir)
		templates = append(templates, loaded...)
		errs = append(errs, loadErrs...)
	}
	embedded, embeddedErrs := LoadEmbedded()
	templates = append(templates, embedded...)
	errs = append(errs, embeddedErrs...)
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].ID < templates[j].ID
	})
	return templates, errs
}

// DefaultTemplateDirs returns the directories searched when the caller
// names none.
//
// The environment variable comes first because it is the explicit one. The
// embedded set is not a directory and is added by LoadDefault, so a caller
// that wants the corpus always gets it even with no directory installed.
func DefaultTemplateDirs() []string {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv("CERAA_NUCLEI_TEMPLATES")); env != "" {
		// The explicit one is kept even when it does not exist: a path the
		// operator named is reported, not quietly dropped.
		dirs = append(dirs, env)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidate := filepath.Join(home, "nuclei-templates")
		if isDir(candidate) {
			dirs = append(dirs, candidate)
		}
	}
	if isDir("/workspace/data/nuclei-templates") {
		dirs = append(dirs, "/workspace/data/nuclei-templates")
	}
	return dirs
}

// isTemplateFile reports whether a name is a template file.
func isTemplateFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

// isDir reports whether a path is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

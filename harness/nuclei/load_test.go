package nuclei

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplateFile writes a template into a directory.
func writeTemplateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadKeepsTheGoodTemplatesWhenOneIsBroken: one broken file in a corpus
// must not cost the other templates.
func TestLoadKeepsTheGoodTemplatesWhenOneIsBroken(t *testing.T) {
	dir := t.TempDir()
	writeTemplateFile(t, filepath.Join(dir, "good.yaml"), `id: good-one
info:
  name: Good One
  severity: info
http:
  - method: GET
    path: ["{{BaseURL}}/"]
    matchers:
      - type: status
        status: [200]
`)
	writeTemplateFile(t, filepath.Join(dir, "broken.yaml"), "id: broken\nhttp:\n  - path: [\n")
	writeTemplateFile(t, filepath.Join(dir, "network.yaml"), `id: tcp-one
info:
  name: TCP One
  severity: info
network:
  - host: "{{BaseURL}}"
`)
	writeTemplateFile(t, filepath.Join(dir, "notes.txt"), "not a template")

	templates, errs := Load(dir)
	if len(templates) != 1 {
		t.Fatalf("loaded %d templates, want the one that parses: %v", len(templates), errs)
	}
	if templates[0].ID != "good-one" {
		t.Errorf("the loaded template is %q, want good-one", templates[0].ID)
	}
	if templates[0].Path == "" {
		t.Error("the loaded template does not carry the file it came from")
	}
	if len(errs) != 2 {
		t.Fatalf("collected %d errors, want one per unrunnable template: %v", len(errs), errs)
	}
	joined := errs[0].Error() + " " + errs[1].Error()
	if !strings.Contains(joined, "broken.yaml") || !strings.Contains(joined, "network.yaml") {
		t.Errorf("the errors do not name the files: %v", errs)
	}
}

// TestLoadReadsASingleFile: a file is a template set of one.
func TestLoadReadsASingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one.yaml")
	writeTemplateFile(t, path, `id: single
info:
  name: Single
  severity: info
http:
  - method: GET
    path: ["{{BaseURL}}/"]
    matchers:
      - type: status
        status: [200]
`)
	templates, errs := Load(path)
	if len(errs) != 0 {
		t.Fatalf("a good file produced errors: %v", errs)
	}
	if len(templates) != 1 || templates[0].ID != "single" {
		t.Errorf("loaded %v", templates)
	}
}

// TestLoadReportsAMissingPath: the path the operator named is in the error.
func TestLoadReportsAMissingPath(t *testing.T) {
	_, errs := Load(filepath.Join(t.TempDir(), "nowhere"))
	if len(errs) != 1 {
		t.Fatalf("collected %d errors, want 1", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "nowhere") {
		t.Errorf("the error does not name the path: %v", errs[0])
	}
}

// TestLoadEmbeddedCarriesAUsableCorpus: the embedded set is the checks that
// matter most, and every one of them has to be a real template.
func TestLoadEmbeddedCarriesAUsableCorpus(t *testing.T) {
	templates, errs := LoadEmbedded()
	if len(errs) != 0 {
		t.Fatalf("the embedded set produced errors: %v", errs)
	}
	if len(templates) < 12 {
		t.Fatalf("the embedded set holds %d templates, want at least 12", len(templates))
	}
	seen := map[string]bool{}
	for _, tpl := range templates {
		if tpl.ID == "" {
			t.Error("an embedded template has no id")
		}
		if seen[tpl.ID] {
			t.Errorf("the embedded id %q is used twice", tpl.ID)
		}
		seen[tpl.ID] = true
		if tpl.Info.Name == "" || tpl.Info.Severity == "" {
			t.Errorf("the embedded template %q has no name or no severity: %+v", tpl.ID, tpl.Info)
		}
		if len(tpl.HTTP) == 0 {
			t.Errorf("the embedded template %q has no http request", tpl.ID)
			continue
		}
		if len(tpl.HTTP[0].Matchers) == 0 {
			t.Errorf("the embedded template %q has no matchers", tpl.ID)
		}
	}
	for _, want := range []string{
		"exposed-git-directory", "exposed-env-file", "exposed-package-json",
		"exposed-wrangler-toml", "robots-txt-present", "missing-hsts-header",
		"missing-x-frame-options", "missing-content-security-policy",
		"cors-origin-reflection", "login-form-password-field", "phpinfo-page",
		"exposed-backup-file", "exposed-directory-listing", "exposed-openapi-document",
	} {
		if !seen[want] {
			t.Errorf("the embedded set has no %q", want)
		}
	}
}

// TestDefaultTemplateDirsPrefersTheEnvironmentVariable: the explicit path
// comes first.
func TestDefaultTemplateDirsPrefersTheEnvironmentVariable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERAA_NUCLEI_TEMPLATES", dir)
	dirs := DefaultTemplateDirs()
	if len(dirs) == 0 {
		t.Fatal("the default directories are empty")
	}
	if dirs[0] != dir {
		t.Errorf("the first default directory is %q, want the environment one %q", dirs[0], dir)
	}
}

// TestLoadDefaultAlwaysCarriesTheEmbeddedSet: a machine with no corpus
// installed still runs something useful.
func TestLoadDefaultAlwaysCarriesTheEmbeddedSet(t *testing.T) {
	t.Setenv("CERAA_NUCLEI_TEMPLATES", filepath.Join(t.TempDir(), "nothing-here"))
	templates, _ := LoadDefault()
	if len(templates) < 12 {
		t.Errorf("LoadDefault returned %d templates, want at least the embedded set", len(templates))
	}
}

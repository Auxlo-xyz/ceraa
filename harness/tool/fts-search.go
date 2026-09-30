package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ceraa/harness/operation"
	"ceraa/harness/search"
)

// FTSSearchTool provides indexed full-text search via SQLite FTS5.
type FTSSearchTool struct {
	// DBPath is the SQLite index location. If empty, uses DefaultPath().
	DBPath string
}

func (f *FTSSearchTool) Name() string { return "fts_search" }
func (f *FTSSearchTool) Description() string {
	return "Indexed full-text search via SQLite FTS5. Returns ranked candidate files " +
		"without scanning the workspace. Pass path to restrict results to one " +
		"directory; the index is workspace-wide, so an unscoped search returns " +
		"matches from every project."
}
func (f *FTSSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "FTS5 query: terms, phrase \"session persistence\", prefix session*, NEAR, AND/OR"},
			"limit": map[string]any{"type": "integer", "description": "Max results (default 20)"},
			"index": map[string]any{"type": "boolean", "description": "If true, re-index workspace before searching"},
			"path":  map[string]any{"type": "string", "description": "Restrict results to this directory subtree, and index it when indexing. Omit to search the whole workspace."},
		},
		"required": []string{"query"},
	}
}
func (f *FTSSearchTool) Translate(args map[string]any) (operation.Spec, error) {
	q, ok := args["query"].(string)
	if !ok || strings.TrimSpace(q) == "" {
		return operation.Spec{}, fmt.Errorf("query is required")
	}
	spec := operation.Spec{
		Type: "fts_search",
		Args: map[string]any{"query": q},
	}
	if v, ok := args["limit"]; ok {
		spec.Args["limit"] = v
	}
	if v, ok := args["index"]; ok {
		spec.Args["index"] = v
	}
	if v, ok := args["path"]; ok {
		spec.Args["path"] = v
	}
	if f.DBPath != "" {
		spec.Args["db_path"] = f.DBPath
	}
	return spec, nil
}
func (f *FTSSearchTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// FTSSearchRunner executes fts_search operations.
type FTSSearchRunner struct {
	DBPath string
}

func (r *FTSSearchRunner) Run(spec operation.Spec) (string, error) {
	query, _ := spec.Args["query"].(string)
	limit := 20
	if v, ok := spec.Args["limit"]; ok {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case int:
			limit = n
		}
	}
	dbPath, _ := spec.Args["db_path"].(string)
	if dbPath == "" {
		dbPath = r.DBPath
	}
	if dbPath == "" {
		dbPath = search.DefaultPath()
	}
	// The scope is read once and used for both jobs: what gets indexed, and what
	// gets returned. It used to drive only the indexing, so a caller passing a
	// directory got workspace-wide results back and no indication why — the
	// schema describes path as the root to index, which reads as scope.
	scope, _ := spec.Args["path"].(string)
	if scope != "" {
		if abs, err := filepath.Abs(scope); err == nil {
			scope = abs
		}
	}
	// Optionally re-index
	if doIndex, _ := spec.Args["index"].(bool); doIndex {
		root := scope
		if root == "" {
			root = "."
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		idx, err := search.Open(dbPath)
		if err != nil {
			return "", err
		}
		_, _ = idx.IndexDir(root)
		_ = idx.Close()
	}
	idx, err := search.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer idx.Close()

	// Auto-index on first use if empty
	var cnt int
	_ = idx.DB().QueryRow(`SELECT count(*) FROM files`).Scan(&cnt)
	if cnt == 0 {
		root := scope
		if root == "" {
			if cwd, err := os.Getwd(); err == nil {
				root = cwd
			} else {
				root = "."
			}
		}
		_, _ = idx.IndexDir(root)
	}

	results, err := idx.SearchIn(query, scope, limit)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No matches for: " + query, nil
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s (rank %.2f)\n", i+1, r.Path, r.Rank)
	}
	return b.String(), nil
}

package search

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"ceraa/harness/document"
)

// Index is a SQLite FTS5 backed search index.
type Index struct {
	db   *sql.DB
	path string
}

// Result is a single FTS search hit.
type Result struct {
	Path string
	Rank float64
}

// Open opens or creates an FTS5 index at dbPath.
func Open(dbPath string) (*Index, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// busy_timeout matters once indexing happens in the background: SQLite allows
	// one writer at a time, and without it a concurrent writer fails immediately
	// with SQLITE_BUSY rather than waiting. That turns a harmless overlap between a
	// background index and a search into a hard error the user sees.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(dbPath, 0o600)
	idx := &Index{db: db, path: dbPath}
	if err := idx.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

func (idx *Index) init() error {
	_, err := idx.db.Exec(`
		CREATE TABLE IF NOT EXISTS files(
			id INTEGER PRIMARY KEY,
			path TEXT UNIQUE NOT NULL,
			mtime INTEGER NOT NULL,
			size INTEGER NOT NULL,
			hash TEXT
		);
		CREATE VIRTUAL TABLE IF NOT EXISTS content USING fts5(
			path, text, tokenize='unicode61'
		);
	`)
	return err
}

// Close closes the index.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// DB exposes underlying sql.DB for advanced queries.
func (idx *Index) DB() *sql.DB { return idx.db }

// DefaultPath returns the default index location.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ".ceraa-index.db"
	}
	return filepath.Join(home, ".cache", "ceraa", "index.db")
}

func ProjectIndexPath(workspaceRoot, projectRoot string) string {
	workspace, err := filepath.Abs(workspaceRoot)
	if err != nil {
		workspace = workspaceRoot
	}
	project, err := filepath.Abs(projectRoot)
	if err != nil {
		project = projectRoot
	}
	hash := sha256.Sum256([]byte(project))
	return filepath.Join(workspace, ".ceraa", "index", hex.EncodeToString(hash[:])+".db")
}

// IndexFile indexes a single file. Content is the full file text.
func (idx *Index) IndexFile(path string, content string) error {
	info, err := os.Stat(path)
	var mtime int64
	var size int64
	if err == nil {
		mtime = info.ModTime().Unix()
		size = info.Size()
	} else {
		mtime = time.Now().Unix()
		size = int64(len(content))
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Upsert file row
	_, err = tx.Exec(`INSERT INTO files(path, mtime, size) VALUES(?,?,?)
		ON CONFLICT(path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size`, path, mtime, size)
	if err != nil {
		return err
	}
	// Remove old FTS entry
	if _, err := tx.Exec(`DELETE FROM content WHERE path = ?`, path); err != nil {
		return err
	}
	if strings.TrimSpace(content) != "" {
		if _, err := tx.Exec(`INSERT INTO content(path, text) VALUES(?,?)`, path, content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveFile removes a file from the index.
func (idx *Index) RemoveFile(path string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM content WHERE path = ?`, path); err != nil {
		return err
	}
	return tx.Commit()
}

// indexDocument indexes an archive or office document by what it contains
// rather than what it is. A zip contributes its member names, so a filename
// inside an archive still finds the archive; PDF, DOCX and PPTX contribute
// extracted text. Anything that fails to extract is removed from the index,
// matching the old behaviour of skipping what cannot be read.
func (idx *Index) indexDocument(path string, kind document.Kind, seen map[string]bool) error {
	if kind == document.KindArchive {
		members, err := document.ExtractArchive(path)
		if err != nil {
			_ = idx.RemoveFile(path)
			return nil
		}
		var names []string
		for _, m := range members {
			names = append(names, m.Path)
		}
		if err := idx.IndexFile(path, strings.Join(names, "\n")); err != nil {
			return err
		}
		// The walker listed this directory before the extraction created the
		// member files, so it will never visit them on this pass. Index the
		// extracted text members here instead of waiting for the next run,
		// or a file inside an archive is unsearchable until someone reindexes.
		for _, m := range members {
			if m.Skipped != "" || ignoredFile(filepath.Base(m.Path)) {
				continue
			}
			info, infoErr := os.Stat(m.Path)
			if infoErr != nil || info.IsDir() || info.Size() > 5*1024*1024 {
				continue
			}
			data, readErr := os.ReadFile(m.Path)
			if readErr != nil || isBinary(data) {
				continue
			}
			if indexErr := idx.IndexFile(m.Path, string(data)); indexErr != nil {
				return indexErr
			}
			// The walker never visits extracted members, so without this the
			// prune at the end of the run deletes what was just indexed.
			seen[m.Path] = true
		}
		return nil
	}
	text, err := document.ExtractText(path)
	if err != nil {
		_ = idx.RemoveFile(path)
		return nil
	}
	return idx.IndexFile(path, text)
}

// IndexDir walks root and indexes text files incrementally.
func (idx *Index) IndexDir(root string) (int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	count := 0
	seen := make(map[string]bool)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if ignoredDirectory(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignoredFile(d.Name()) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".tar", ".gz", ".bin", ".exe", ".so", ".db", ".sqlite":
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.Size() > 5*1024*1024 {
			return nil
		}
		seen[path] = true
		var storedMtime int64
		var storedSize int64
		err = idx.db.QueryRow(`SELECT mtime, size FROM files WHERE path = ?`, path).Scan(&storedMtime, &storedSize)
		if err == nil && storedMtime == info.ModTime().Unix() && storedSize == info.Size() {
			return nil
		}
		// Documents index as their extracted text, not their bytes. A PDF of
		// binary would otherwise fail the binary check below and vanish from
		// search; a zip indexes its member names, so a filename inside an
		// archive still finds the archive. Extraction failures fall back to
		// skipping, because an unscanned file was the old behaviour and is
		// still better than indexing mojibake.
		if kind, ok := document.KindForPath(path); ok {
			indexErr := idx.indexDocument(path, kind, seen)
			if indexErr != nil {
				return nil
			}
			count++
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if isBinary(data) {
			_ = idx.RemoveFile(path)
			return nil
		}
		if indexErr := idx.IndexFile(path, string(data)); indexErr != nil {
			return nil
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	if err := idx.prune(seen); err != nil {
		return count, err
	}
	return count, nil
}

func ignoredDirectory(name string) bool {
	switch name {
	case ".git", ".cache", "node_modules", "bin", ".ceraa", ".venv", "vendor", "dist", "build":
		return true
	default:
		return false
	}
}

func ignoredFile(name string) bool {
	lower := strings.ToLower(name)
	// The extraction marker is bookkeeping, not content. Indexing it would
	// attach bare numbers to every archive directory.
	if lower == ".ceraa-extracted" {
		return true
	}
	return lower == ".env" || strings.HasPrefix(lower, ".env.") || lower == "credentials" || lower == "secrets.json"
}

func (idx *Index) prune(seen map[string]bool) error {
	rows, err := idx.db.Query(`SELECT path FROM files`)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err == nil {
			paths = append(paths, path)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, path := range paths {
		if !seen[path] {
			if err := idx.RemoveFile(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func isBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	sample := data
	if len(sample) > 8000 {
		sample = sample[:8000]
	}
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	return false
}

// Search performs an FTS5 query and returns ranked results.
func (idx *Index) Search(query string, limit int) ([]Result, error) {
	return idx.SearchIn(query, "", limit)
}

// SearchIn performs an FTS5 query restricted to one directory subtree.
//
// The restriction is in the SQL rather than a filter over the returned rows,
// because the LIMIT is applied by the query. Filtering afterwards would cap the
// scan at `limit` rows first and then throw most of them away, so a scoped
// search would quietly return fewer results than asked for the moment the
// workspace held more than `limit` matching files.
func (idx *Index) SearchIn(query, dir string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 20
	}
	query = sanitizeQuery(query)
	if query == "" {
		return nil, nil
	}
	// A trailing separator is what makes this a subtree rather than a string
	// prefix: without it, scoping to /workspace/auxlo would also match
	// /workspace/auxlo-xyz.
	prefix := dirPrefix(dir)
	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = idx.db.Query(`SELECT path, rank FROM content WHERE content MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	} else {
		rows, err = idx.db.Query(
			`SELECT path, rank FROM content WHERE content MATCH ? AND path LIKE ? ESCAPE '\' ORDER BY rank LIMIT ?`,
			query, prefix+`%`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.Path, &r.Rank); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// dirPrefix normalises a scope to an absolute directory prefix ending in a
// separator, ready to be a LIKE pattern. Empty means unscoped.
func dirPrefix(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	if dir == string(filepath.Separator) {
		return dir
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	// LIKE wildcards in a path are legal on disk and would silently widen the
	// scope, so they are escaped to match the ESCAPE clause above.
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(dir)
}

// sanitizeQuery escapes FTS5 special characters and ensures valid query.
func sanitizeQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	// If query already contains FTS operators, keep as is after basic validation
	// Otherwise treat as AND query over terms
	if strings.Contains(q, `"`) || strings.Contains(q, " NEAR ") || strings.Contains(q, " AND ") || strings.Contains(q, " OR ") {
		return q
	}
	// Escape double quotes and single quotes
	q = strings.ReplaceAll(q, `"`, `""`)
	// Split into terms and join with AND for better precision
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return ""
	}
	if len(terms) == 1 {
		return terms[0]
	}
	// For multi-term, use implicit AND via space (FTS5 default is AND for space-separated)
	return strings.Join(terms, " ")
}

// NeedsReindex checks if file at path has changed since last index.
func (idx *Index) NeedsReindex(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	var mtime int64
	var size int64
	err = idx.db.QueryRow(`SELECT mtime, size FROM files WHERE path = ?`, path).Scan(&mtime, &size)
	if err != nil {
		return true
	}
	return mtime != info.ModTime().Unix() || size != info.Size()
}

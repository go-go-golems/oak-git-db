package oakgitdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"

	_ "github.com/mattn/go-sqlite3"
)

type BuildOptions struct {
	RepoDirs []string
	BaseRef string
	HeadRef string
	OutPath string

	OakSources  []string
	OakGlob     []string
	OakWithBody bool

	GoPackages      []string
	IncludeExternal bool
}

func Build(ctx context.Context, opts BuildOptions) error {
	if len(opts.RepoDirs) == 0 {
		return errors.New("at least one RepoDir is required")
	}
	if opts.BaseRef == "" {
		opts.BaseRef = "origin/main"
	}
	if opts.HeadRef == "" {
		opts.HeadRef = "HEAD"
	}
	if opts.OutPath == "" {
		return errors.New("OutPath is required")
	}
	if len(opts.OakSources) == 0 {
		opts.OakSources = []string{"cmd", "pkg", "misc"}
	}
	if len(opts.OakGlob) == 0 {
		opts.OakGlob = []string{"*.go"}
	}
	if len(opts.GoPackages) == 0 {
		opts.GoPackages = []string{"./..."}
	}

	db, err := sql.Open("sqlite3", opts.OutPath)
	if err != nil {
		return errors.Wrap(err, "open sqlite db")
	}
	defer db.Close()

	if err := applyPragmas(db); err != nil {
		return err
	}
	if err := createSchema(db); err != nil {
		return err
	}

	now := time.Now().Format(time.RFC3339)
	for _, repo := range opts.RepoDirs {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		repoDir, err := filepath.Abs(repo)
		if err != nil {
			return errors.Wrap(err, "abs repo dir")
		}

		headSHA, err := gitString(ctx, repoDir, "rev-parse", opts.HeadRef)
		if err != nil {
			return errors.Wrapf(err, "resolve head ref (repo=%s)", repoDir)
		}
		baseSHA, err := gitString(ctx, repoDir, "merge-base", opts.BaseRef, headSHA)
		if err != nil {
			return errors.Wrapf(err, "resolve merge-base (repo=%s)", repoDir)
		}

		originURL, _ := gitString(ctx, repoDir, "remote", "get-url", "origin")
		repoName := filepath.Base(repoDir)

		repoID, err := insertRepo(ctx, db, repoName, repoDir, originURL, now)
		if err != nil {
			return err
		}

		baseSnapshotID, err := insertSnapshot(ctx, db, repoID, "base", opts.BaseRef, baseSHA, now)
		if err != nil {
			return err
		}
		headSnapshotID, err := insertSnapshot(ctx, db, repoID, "head", opts.HeadRef, headSHA, now)
		if err != nil {
			return err
		}

		prID, err := insertPR(ctx, db, repoID, baseSnapshotID, headSnapshotID, baseSHA, opts.BaseRef, opts.HeadRef, now)
		if err != nil {
			return err
		}

		if err := ingestGitPR(ctx, db, repoDir, repoID, prID, baseSHA, headSHA); err != nil {
			return err
		}

		if err := ingestOakSnapshot(ctx, db, repoDir, repoID, baseSnapshotID, baseSHA, opts.OakSources, opts.OakGlob, opts.OakWithBody); err != nil {
			return err
		}
		if err := ingestOakSnapshot(ctx, db, repoDir, repoID, headSnapshotID, headSHA, opts.OakSources, opts.OakGlob, opts.OakWithBody); err != nil {
			return err
		}

		if err := ingestGoSnapshot(ctx, db, repoDir, repoID, headSnapshotID, opts.GoPackages, opts.IncludeExternal); err != nil {
			return err
		}
	}

	return nil
}

func applyPragmas(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return errors.Wrap(err, "pragma journal_mode")
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		return errors.Wrap(err, "pragma synchronous")
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		return errors.Wrap(err, "pragma foreign_keys")
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repo (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  root_path TEXT NOT NULL UNIQUE,
  remote_origin TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshot (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  ref TEXT NOT NULL,
  sha TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(repo_id) REFERENCES repo(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_snapshot_unique ON snapshot(repo_id, name);
CREATE INDEX IF NOT EXISTS idx_snapshot_sha ON snapshot(sha);

CREATE TABLE IF NOT EXISTS pr (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL,
  base_snapshot_id INTEGER NOT NULL,
  head_snapshot_id INTEGER NOT NULL,
  merge_base_sha TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  head_ref TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(repo_id) REFERENCES repo(id),
  FOREIGN KEY(base_snapshot_id) REFERENCES snapshot(id),
  FOREIGN KEY(head_snapshot_id) REFERENCES snapshot(id)
);

CREATE TABLE IF NOT EXISTS git_commit (
  repo_id INTEGER NOT NULL,
  sha TEXT NOT NULL,
  parents TEXT,
  author_name TEXT,
  author_email TEXT,
  authored_at TEXT,
  committer_name TEXT,
  committer_email TEXT,
  committed_at TEXT,
  subject TEXT,
  body TEXT,
  PRIMARY KEY (repo_id, sha),
  FOREIGN KEY(repo_id) REFERENCES repo(id)
);

CREATE TABLE IF NOT EXISTS pr_commit (
  pr_id INTEGER NOT NULL,
  repo_id INTEGER NOT NULL,
  sha TEXT NOT NULL,
  ord INTEGER NOT NULL,
  PRIMARY KEY (pr_id, sha),
  FOREIGN KEY(pr_id) REFERENCES pr(id),
  FOREIGN KEY(repo_id, sha) REFERENCES git_commit(repo_id, sha)
);

CREATE TABLE IF NOT EXISTS path (
  id INTEGER PRIMARY KEY,
  repo_id INTEGER NOT NULL,
  path TEXT NOT NULL,
  FOREIGN KEY(repo_id) REFERENCES repo(id),
  UNIQUE(repo_id, path)
);

CREATE TABLE IF NOT EXISTS pr_file (
  pr_id INTEGER NOT NULL,
  path_id INTEGER NOT NULL,
  change_type TEXT NOT NULL,
  old_path_id INTEGER,
  rename_score INTEGER,
  additions INTEGER,
  deletions INTEGER,
  PRIMARY KEY (pr_id, path_id),
  FOREIGN KEY(pr_id) REFERENCES pr(id),
  FOREIGN KEY(path_id) REFERENCES path(id),
  FOREIGN KEY(old_path_id) REFERENCES path(id)
);

CREATE INDEX IF NOT EXISTS idx_pr_file_change_type ON pr_file(pr_id, change_type);

CREATE TABLE IF NOT EXISTS oak_match (
  snapshot_id INTEGER NOT NULL,
  path_id INTEGER NOT NULL,
  query TEXT NOT NULL,
  capture TEXT NOT NULL,
  node_type TEXT,
  text TEXT,
  start_byte INTEGER,
  end_byte INTEGER,
  start_row INTEGER,
  start_col INTEGER,
  end_row INTEGER,
  end_col INTEGER,
  PRIMARY KEY (snapshot_id, path_id, query, capture, start_byte, end_byte),
  FOREIGN KEY(snapshot_id) REFERENCES snapshot(id),
  FOREIGN KEY(path_id) REFERENCES path(id)
);

CREATE INDEX IF NOT EXISTS idx_oak_match_lookup ON oak_match(snapshot_id, path_id, query, capture);
CREATE INDEX IF NOT EXISTS idx_oak_match_text ON oak_match(snapshot_id, text);

CREATE TABLE IF NOT EXISTS go_symbol (
  id INTEGER PRIMARY KEY,
  snapshot_id INTEGER NOT NULL,
  symbol_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  pkg_path TEXT,
  name TEXT NOT NULL,
  recv TEXT,
  signature TEXT,
  path_id INTEGER,
  start_line INTEGER,
  start_col INTEGER,
  end_line INTEGER,
  end_col INTEGER,
  doc TEXT,
  is_exported INTEGER NOT NULL,
  is_external INTEGER NOT NULL,
  FOREIGN KEY(snapshot_id) REFERENCES snapshot(id),
  FOREIGN KEY(path_id) REFERENCES path(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_go_symbol_key ON go_symbol(snapshot_id, symbol_key);
CREATE INDEX IF NOT EXISTS idx_go_symbol_name ON go_symbol(snapshot_id, name);
CREATE INDEX IF NOT EXISTS idx_go_symbol_pkg ON go_symbol(snapshot_id, pkg_path);

CREATE TABLE IF NOT EXISTS go_ref (
  snapshot_id INTEGER NOT NULL,
  from_symbol_id INTEGER NOT NULL,
  to_symbol_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  path_id INTEGER,
  line INTEGER,
  col INTEGER,
  PRIMARY KEY (snapshot_id, from_symbol_id, to_symbol_id, kind, path_id, line, col),
  FOREIGN KEY(snapshot_id) REFERENCES snapshot(id),
  FOREIGN KEY(from_symbol_id) REFERENCES go_symbol(id),
  FOREIGN KEY(to_symbol_id) REFERENCES go_symbol(id),
  FOREIGN KEY(path_id) REFERENCES path(id)
);

CREATE INDEX IF NOT EXISTS idx_go_ref_to ON go_ref(snapshot_id, to_symbol_id, kind);
CREATE INDEX IF NOT EXISTS idx_go_ref_from ON go_ref(snapshot_id, from_symbol_id, kind);
`

func createSchema(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return errors.Wrap(err, "create schema")
	}

	var existing string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&existing)
	if err == nil && existing != "" && existing != "2" {
		return errors.Errorf("incompatible schema_version=%s (expected 2); delete the DB and rebuild", existing)
	}

	if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES ('schema_version','2');`); err != nil {
		return errors.Wrap(err, "set schema_version")
	}
	return nil
}

func insertRepo(ctx context.Context, db *sql.DB, name, rootPath, originURL, createdAt string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO repo(name,root_path,remote_origin,created_at) VALUES (?,?,?,?)`, name, rootPath, originURL, createdAt)
	if err != nil {
		return 0, errors.Wrap(err, "insert repo")
	}
	return res.LastInsertId()
}

func insertSnapshot(ctx context.Context, db *sql.DB, repoID int64, name, ref, sha, createdAt string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO snapshot(repo_id,name,ref,sha,created_at) VALUES (?,?,?,?,?)`, repoID, name, ref, sha, createdAt)
	if err != nil {
		return 0, errors.Wrap(err, "insert snapshot")
	}
	return res.LastInsertId()
}

func insertPR(ctx context.Context, db *sql.DB, repoID, baseSnapshotID, headSnapshotID int64, mergeBaseSHA, baseRef, headRef, createdAt string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO pr(repo_id,base_snapshot_id,head_snapshot_id,merge_base_sha,base_ref,head_ref,created_at) VALUES (?,?,?,?,?,?,?)`,
		repoID, baseSnapshotID, headSnapshotID, mergeBaseSHA, baseRef, headRef, createdAt)
	if err != nil {
		return 0, errors.Wrap(err, "insert pr")
	}
	return res.LastInsertId()
}

func ingestGitPR(ctx context.Context, db *sql.DB, repoDir string, repoID, prID int64, baseSHA, headSHA string) error {
	// Commits in range.
	commitsRaw, err := gitBytes(ctx, repoDir, "log", "--reverse", "--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%s%x00%b%x00", baseSHA+".."+headSHA)
	if err != nil {
		return errors.Wrap(err, "git log range")
	}
	commitParts := bytes.Split(commitsRaw, []byte{0})
	type commitRow struct {
		SHA          string
		Parents      string
		AuthorName   string
		AuthorEmail  string
		AuthoredAt   string
		CommitName   string
		CommitEmail  string
		CommittedAt  string
		Subject      string
		Body         string
	}
	var rows []commitRow
	for i := 0; i+9 < len(commitParts); i += 10 {
		sha := strings.TrimSpace(string(commitParts[i]))
		if sha == "" {
			continue
		}
		rows = append(rows, commitRow{
			SHA:         sha,
			Parents:     strings.TrimSpace(string(commitParts[i+1])),
			AuthorName:  string(commitParts[i+2]),
			AuthorEmail: string(commitParts[i+3]),
			AuthoredAt:  unixToRFC3339(string(commitParts[i+4])),
			CommitName:  string(commitParts[i+5]),
			CommitEmail: string(commitParts[i+6]),
			CommittedAt: unixToRFC3339(string(commitParts[i+7])),
			Subject:     string(commitParts[i+8]),
			Body:        strings.TrimSpace(string(commitParts[i+9])),
		})
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	commitStmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO git_commit(repo_id,sha,parents,author_name,author_email,authored_at,committer_name,committer_email,committed_at,subject,body) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare git_commit")
	}
	defer commitStmt.Close()

	prCommitStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO pr_commit(pr_id,repo_id,sha,ord) VALUES (?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare pr_commit")
	}
	defer prCommitStmt.Close()

	for i, r := range rows {
		if _, err := commitStmt.ExecContext(ctx, repoID, r.SHA, r.Parents, r.AuthorName, r.AuthorEmail, r.AuthoredAt, r.CommitName, r.CommitEmail, r.CommittedAt, r.Subject, r.Body); err != nil {
			return errors.Wrap(err, "insert git_commit")
		}
		if _, err := prCommitStmt.ExecContext(ctx, prID, repoID, r.SHA, i); err != nil {
			return errors.Wrap(err, "insert pr_commit")
		}
	}

	// Files changed in range (name-status with renames/copies).
	statusRaw, err := gitBytes(ctx, repoDir, "diff", "--name-status", "-z", "-M", baseSHA+".."+headSHA)
	if err != nil {
		return errors.Wrap(err, "git diff --name-status")
	}
	statusEntries, err := parseNameStatusZ(statusRaw)
	if err != nil {
		return errors.Wrap(err, "parse name-status")
	}

	prFileStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO pr_file(pr_id,path_id,change_type,old_path_id,rename_score,additions,deletions) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare pr_file")
	}
	defer prFileStmt.Close()

	for _, e := range statusEntries {
		pathID, err := getOrCreatePathID(ctx, tx, repoID, e.Path)
		if err != nil {
			return err
		}
		var oldID any = nil
		if e.OldPath != "" {
			oid, err := getOrCreatePathID(ctx, tx, repoID, e.OldPath)
			if err != nil {
				return err
			}
			oldID = oid
		}
		var score any = nil
		if e.Score != 0 {
			score = e.Score
		}
		if _, err := prFileStmt.ExecContext(ctx, prID, pathID, e.ChangeType, oldID, score, nil, nil); err != nil {
			return errors.Wrap(err, "insert pr_file")
		}
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit tx")
	}
	return nil
}

func getOrCreatePathID(ctx context.Context, q querier, repoID int64, path string) (int64, error) {
	var id int64
	if err := q.QueryRowContext(ctx, `SELECT id FROM path WHERE repo_id=? AND path=?`, repoID, path).Scan(&id); err == nil {
		return id, nil
	}
	res, err := q.ExecContext(ctx, `INSERT INTO path(repo_id,path) VALUES (?,?)`, repoID, path)
	if err != nil {
		return 0, errors.Wrap(err, "insert path")
	}
	return res.LastInsertId()
}

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type nameStatusEntry struct {
	ChangeType string
	Score      int
	OldPath    string
	Path       string
}

func parseNameStatusZ(b []byte) ([]nameStatusEntry, error) {
	parts := bytes.Split(b, []byte{0})
	var out []nameStatusEntry
	for i := 0; i < len(parts); {
		raw := string(parts[i])
		i++
		if raw == "" {
			continue
		}
		code := raw[:1]
		score := 0
		if len(raw) > 1 {
			_, _ = fmt.Sscanf(raw[1:], "%d", &score)
		}

		switch code {
		case "R", "C":
			if i+1 >= len(parts) {
				return nil, errors.New("truncated rename/copy entry")
			}
			oldPath := string(parts[i])
			newPath := string(parts[i+1])
			i += 2
			out = append(out, nameStatusEntry{
				ChangeType: code,
				Score:      score,
				OldPath:    oldPath,
				Path:       newPath,
			})
		default:
			if i >= len(parts) {
				return nil, errors.New("truncated entry")
			}
			path := string(parts[i])
			i++
			out = append(out, nameStatusEntry{
				ChangeType: code,
				Score:      score,
				Path:       path,
			})
		}
	}
	return out, nil
}

func ingestOakSnapshot(ctx context.Context, db *sql.DB, repoDir string, repoID, snapshotID int64, sha string, sources []string, globs []string, withBody bool) error {
	// Extract snapshot to a temp dir for stable analysis.
	tmpDir, err := os.MkdirTemp("", "oakgitdb-oak-"+sha[:8]+"-")
	if err != nil {
		return errors.Wrap(err, "mk temp dir")
	}
	defer os.RemoveAll(tmpDir)

	if err := extractGitTree(ctx, repoDir, sha, tmpDir); err != nil {
		return errors.Wrap(err, "extract git tree")
	}

	oakMatches, err := runOakDefinitions(ctx, tmpDir, sources, globs, withBody)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO oak_match(snapshot_id,path_id,query,capture,node_type,text,start_byte,end_byte,start_row,start_col,end_row,end_col) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare oak_match")
	}
	defer stmt.Close()

	for _, m := range oakMatches {
		pathID, err := getOrCreatePathID(ctx, tx, repoID, filepath.ToSlash(m.File))
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, snapshotID, pathID, m.Query, m.Capture, m.Type, m.Text, m.StartByte, m.EndByte, m.StartRow, m.StartColumn, m.EndRow, m.EndColumn); err != nil {
			return errors.Wrap(err, "insert oak_match")
		}
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit tx")
	}
	return nil
}

type OakMatch struct {
	File        string `json:"file"`
	Query       string `json:"query"`
	Capture     string `json:"capture"`
	Type        string `json:"type"`
	Text        string `json:"text"`
	StartByte   int    `json:"startByte"`
	EndByte     int    `json:"endByte"`
	StartRow    int    `json:"startRow"`
	StartColumn int    `json:"startColumn"`
	EndRow      int    `json:"endRow"`
	EndColumn   int    `json:"endColumn"`
}

func runOakDefinitions(ctx context.Context, dir string, sources []string, globs []string, withBody bool) ([]OakMatch, error) {
	files, err := collectMatchingFiles(dir, sources, globs)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return []OakMatch{}, nil
	}

	const chunkSize = 200
	var out []OakMatch
	for i := 0; i < len(files); i += chunkSize {
		end := i + chunkSize
		if end > len(files) {
			end = len(files)
		}
		chunk := files[i:end]

		args := []string{"glaze", "go", "definitions"}
		args = append(args, chunk...)
		if withBody {
			args = append(args, "--with-body")
		}
		args = append(args, "--output", "json")

		cmd := exec.CommandContext(ctx, "oak", args...)
		cmd.Dir = dir
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		// Oak emits warnings on stderr; keep stderr for debugging but do not mix it with stdout JSON.
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, errors.Wrapf(err, "run oak (stderr=%s)", truncate(stderr.String(), 4000))
		}

		var matches []OakMatch
		if err := json.Unmarshal(stdout.Bytes(), &matches); err != nil {
			return nil, errors.Wrapf(err, "parse oak json (stderr=%s)", truncate(stderr.String(), 2000))
		}
		out = append(out, matches...)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func ingestGoSnapshot(ctx context.Context, db *sql.DB, repoDir string, repoID, snapshotID int64, patterns []string, includeExternal bool) error {
	modulePath, err := readModulePath(filepath.Join(repoDir, "go.mod"))
	if err != nil {
		return errors.Wrap(err, "read module path")
	}

	cfg := &packages.Config{
		Context: ctx,
		Dir:     repoDir,
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedModule,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return errors.Wrap(err, "packages.Load")
	}
	if packages.PrintErrors(pkgs) > 0 {
		// Still proceed; oak provides a fallback, but typed analysis may be partial.
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin tx")
	}
	defer func() { _ = tx.Rollback() }()

	symbolInsert, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO go_symbol(snapshot_id,symbol_key,kind,pkg_path,name,recv,signature,path_id,start_line,start_col,end_line,end_col,doc,is_exported,is_external) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare go_symbol insert")
	}
	defer symbolInsert.Close()

	// Keep an in-memory map to resolve symbol_key -> go_symbol.id
	symbolIDByKey := map[string]int64{}

	getSymbolID := func(sym goSymbolRow) (int64, error) {
		if id, ok := symbolIDByKey[sym.SymbolKey]; ok {
			return id, nil
		}

		var pathID any = nil
		if sym.Path != "" {
			pid, err := getOrCreatePathID(ctx, tx, repoID, sym.Path)
			if err != nil {
				return 0, err
			}
			pathID = pid
		}

		if _, err := symbolInsert.ExecContext(ctx,
			snapshotID,
			sym.SymbolKey,
			sym.Kind,
			nullIfEmpty(sym.PkgPath),
			sym.Name,
			nullIfEmpty(sym.Recv),
			nullIfEmpty(sym.Signature),
			pathID,
			nullIfZero(sym.StartLine),
			nullIfZero(sym.StartCol),
			nullIfZero(sym.EndLine),
			nullIfZero(sym.EndCol),
			nullIfEmpty(sym.Doc),
			boolToInt(sym.IsExported),
			boolToInt(sym.IsExternal),
		); err != nil {
			return 0, errors.Wrap(err, "insert go_symbol")
		}

		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM go_symbol WHERE snapshot_id=? AND symbol_key=?`, snapshotID, sym.SymbolKey).Scan(&id); err != nil {
			return 0, errors.Wrap(err, "select go_symbol id")
		}
		symbolIDByKey[sym.SymbolKey] = id
		return id, nil
	}

	refInsert, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO go_ref(snapshot_id,from_symbol_id,to_symbol_id,kind,path_id,line,col) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return errors.Wrap(err, "prepare go_ref insert")
	}
	defer refInsert.Close()

	for _, pkg := range pkgs {
		if pkg.Types == nil || pkg.TypesInfo == nil || pkg.Fset == nil {
			continue
		}

		qualifier := func(p *types.Package) string {
			if p == nil {
				return ""
			}
			return p.Path()
		}

		// Ensure we have a package-level symbol to attribute non-function refs.
		pkgSymbolKey := fmt.Sprintf("%s::pkg::%s", pkg.Types.Path(), pkg.Types.Name())
		pkgSymbolID, err := getSymbolID(goSymbolRow{
			SymbolKey:  pkgSymbolKey,
			Kind:       "pkg",
			PkgPath:    pkg.Types.Path(),
			Name:       pkg.Types.Name(),
			IsExported: true,
			IsExternal: !strings.HasPrefix(pkg.Types.Path(), modulePath),
		})
		if err != nil {
			return err
		}

		// First pass: record declared symbols with best-effort doc/comments and positions.
		for _, file := range pkg.Syntax {
			collectGoDecls(pkg.Fset, repoDir, pkg.TypesInfo, file, func(obj types.Object, kind, doc string, start, end token.Pos) error {
				sym, ok := toGoSymbolRow(pkg.Fset, repoDir, qualifier, modulePath, includeExternal, obj, kind, doc, start, end)
				if !ok {
					return nil
				}
				_, err := getSymbolID(sym)
				return err
			})
		}

		// Second pass: references (focus on call edges; keep other edges limited).
		for _, file := range pkg.Syntax {
			var enclosingStack []types.Object

			astutil.Apply(file, func(c *astutil.Cursor) bool {
				switch node := c.Node().(type) {
				case *ast.FuncDecl:
					if node.Name != nil {
						if obj := pkg.TypesInfo.Defs[node.Name]; obj != nil {
							enclosingStack = append(enclosingStack, obj)
						} else {
							enclosingStack = append(enclosingStack, nil)
						}
					} else {
						enclosingStack = append(enclosingStack, nil)
					}
					return true
				case *ast.CallExpr:
					fromID := pkgSymbolID
					if len(enclosingStack) > 0 && enclosingStack[len(enclosingStack)-1] != nil {
						fromSym, ok := toGoSymbolRow(pkg.Fset, repoDir, qualifier, modulePath, includeExternal, enclosingStack[len(enclosingStack)-1], "", "", token.NoPos, token.NoPos)
						if ok {
							id, err := getSymbolID(fromSym)
							if err != nil {
								return false
							}
							fromID = id
						}
					}

					if obj := calledObject(pkg.TypesInfo, node); obj != nil {
						toSym, ok := toGoSymbolRow(pkg.Fset, repoDir, qualifier, modulePath, includeExternal, obj, "", "", token.NoPos, token.NoPos)
						if ok {
							toID, err := getSymbolID(toSym)
							if err != nil {
								return false
							}

							pos := pkg.Fset.Position(node.Lparen)
							var pathID any = nil
							if rel, ok := relPath(repoDir, pos.Filename); ok {
								pid, err := getOrCreatePathID(ctx, tx, repoID, rel)
								if err != nil {
									return false
								}
								pathID = pid
							}

							if _, err := refInsert.ExecContext(ctx, snapshotID, fromID, toID, "call", pathID, nullIfZero(pos.Line), nullIfZero(pos.Column)); err != nil {
								return false
							}
						}
					}
					return true
				}
				return true
			}, func(c *astutil.Cursor) bool {
				if _, ok := c.Node().(*ast.FuncDecl); ok {
					if len(enclosingStack) > 0 {
						enclosingStack = enclosingStack[:len(enclosingStack)-1]
					}
				}
				return true
			})
		}
	}

	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit tx")
	}
	return nil
}

type goSymbolRow struct {
	SymbolKey  string
	Kind       string
	PkgPath    string
	Name       string
	Recv       string
	Signature  string
	Path       string
	StartLine  int
	StartCol   int
	EndLine    int
	EndCol     int
	Doc        string
	IsExported bool
	IsExternal bool
}

func toGoSymbolRow(
	fset *token.FileSet,
	repoDir string,
	qualifier func(*types.Package) string,
	modulePath string,
	includeExternal bool,
	obj types.Object,
	forcedKind string,
	doc string,
	start token.Pos,
	end token.Pos,
) (goSymbolRow, bool) {
	if obj == nil {
		return goSymbolRow{}, false
	}

	pkgPath := ""
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}
	isExternal := pkgPath != "" && !strings.HasPrefix(pkgPath, modulePath)
	if isExternal && !includeExternal {
		return goSymbolRow{}, false
	}

	kind, recv, symKey := classifyObject(qualifier, obj, forcedKind)
	name := obj.Name()
	if name == "" {
		name = symKey
	}

	signature := ""
	if obj.Type() != nil {
		signature = types.TypeString(obj.Type(), qualifier)
	}

	var rel string
	var startLine, startCol, endLine, endCol int
	if obj.Pos().IsValid() {
		pos := fset.Position(obj.Pos())
		if r, ok := relPath(repoDir, pos.Filename); ok {
			rel = r
			startLine = pos.Line
			startCol = pos.Column
		}
	}
	if start.IsValid() {
		pos := fset.Position(start)
		if r, ok := relPath(repoDir, pos.Filename); ok {
			rel = r
			startLine = pos.Line
			startCol = pos.Column
		}
	}
	if end.IsValid() {
		pos := fset.Position(end)
		if r, ok := relPath(repoDir, pos.Filename); ok {
			rel = r
			endLine = pos.Line
			endCol = pos.Column
		}
	}

	return goSymbolRow{
		SymbolKey:  symKey,
		Kind:       kind,
		PkgPath:    pkgPath,
		Name:       name,
		Recv:       recv,
		Signature:  signature,
		Path:       rel,
		StartLine:  startLine,
		StartCol:   startCol,
		EndLine:    endLine,
		EndCol:     endCol,
		Doc:        doc,
		IsExported: obj.Exported(),
		IsExternal: isExternal,
	}, true
}

func classifyObject(qualifier func(*types.Package) string, obj types.Object, forcedKind string) (kind string, recv string, symbolKey string) {
	if forcedKind != "" {
		kind = forcedKind
	} else {
		switch o := obj.(type) {
		case *types.Func:
			if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
				kind = "method"
				recv = types.TypeString(sig.Recv().Type(), qualifier)
			} else {
				kind = "func"
			}
		case *types.TypeName:
			kind = "type"
		case *types.Const:
			kind = "const"
		case *types.Var:
			if o.IsField() {
				kind = "field"
			} else {
				kind = "var"
			}
		case *types.PkgName:
			kind = "import"
		default:
			kind = "obj"
		}
	}

	pkgPath := ""
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}

	full := obj.Name()
	if f, ok := obj.(*types.Func); ok {
		full = f.FullName()
	}

	if kind == "method" && recv != "" {
		symbolKey = fmt.Sprintf("%s::%s::%s.%s", pkgPath, kind, recv, obj.Name())
	} else {
		symbolKey = fmt.Sprintf("%s::%s::%s", pkgPath, kind, full)
	}

	return kind, recv, symbolKey
}

func calledObject(info *types.Info, call *ast.CallExpr) types.Object {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		if fun.Sel != nil {
			return info.Uses[fun.Sel]
		}
	}
	return nil
}

func collectGoDecls(fset *token.FileSet, repoDir string, info *types.Info, file *ast.File, onObj func(obj types.Object, kind, doc string, start, end token.Pos) error) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			obj := info.Defs[d.Name]
			if obj == nil {
				continue
			}
			doc := ""
			if d.Doc != nil {
				doc = strings.TrimSpace(d.Doc.Text())
			}
			_ = onObj(obj, "", doc, d.Pos(), d.End())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					obj := info.Defs[s.Name]
					if obj == nil {
						continue
					}
					doc := ""
					if s.Doc != nil {
						doc = strings.TrimSpace(s.Doc.Text())
					} else if d.Doc != nil {
						doc = strings.TrimSpace(d.Doc.Text())
					}
					_ = onObj(obj, "", doc, s.Pos(), s.End())
				case *ast.ValueSpec:
					doc := ""
					if s.Doc != nil {
						doc = strings.TrimSpace(s.Doc.Text())
					} else if d.Doc != nil {
						doc = strings.TrimSpace(d.Doc.Text())
					}
					for _, n := range s.Names {
						obj := info.Defs[n]
						if obj == nil {
							continue
						}
						_ = onObj(obj, "", doc, n.Pos(), n.End())
					}
				}
			}
		}
	}
}

func relPath(repoDir string, abs string) (string, bool) {
	if abs == "" {
		return "", false
	}
	rel, err := filepath.Rel(repoDir, abs)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullIfZero(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func collectMatchingFiles(rootDir string, sources []string, globs []string) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}

	isMatch := func(name string) bool {
		for _, g := range globs {
			if ok, _ := filepath.Match(g, name); ok {
				return true
			}
		}
		return false
	}

	for _, src := range sources {
		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}
		abs := filepath.Join(rootDir, filepath.FromSlash(src))
		st, err := os.Stat(abs)
		if err != nil {
			// Ignore missing sources to keep the tool flexible across snapshots.
			continue
		}
		if st.Mode().IsRegular() {
			if isMatch(filepath.Base(abs)) {
				rel, ok := relPath(rootDir, abs)
				if ok {
					if _, exists := seen[rel]; !exists {
						seen[rel] = struct{}{}
						out = append(out, rel)
					}
				}
			}
			continue
		}
		if !st.IsDir() {
			continue
		}

		if err := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if !isMatch(d.Name()) {
				return nil
			}
			rel, ok := relPath(rootDir, path)
			if !ok {
				return nil
			}
			if _, exists := seen[rel]; exists {
				return nil
			}
			seen[rel] = struct{}{}
			out = append(out, rel)
			return nil
		}); err != nil {
			return nil, errors.Wrap(err, "walk sources")
		}
	}

	return out, nil
}

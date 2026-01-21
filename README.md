# oak-git-db

Build a PR-focused SQLite database from:

- `git` (PR range vs base)
- `oak` (tree-sitter definitions for base + head snapshots)
- Go typed analysis (symbols + call edges for head snapshot)

## Layout

- `cmd/oakgitdb/` — CLI entrypoint
- `pkg/oakgitdb/` — DB builder library
- `docs/` — usage + implementation notes

## Quick start

From this repo directory:

```bash
GOCACHE=/tmp/go-build-cache go run ./cmd/oakgitdb build \
  --repo ../geppetto \
  --base origin/main \
  --head HEAD \
  --out /tmp/geppetto-pr.db \
  --oak-sources cmd,pkg,misc \
  --oak-glob '*.go' \
  --packages ./...
```

Then:

```bash
sqlite3 -readonly /tmp/geppetto-pr.db ".tables"
sqlite3 -readonly /tmp/geppetto-pr.db "select change_type,count(*) from pr_file group by change_type;"
```

## Docs

- `docs/usage.md`
- `docs/implementation.md`
- `docs/design.md`

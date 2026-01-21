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
  --repo /path/to/target-repo \
  --base origin/main \
  --head HEAD \
  --out /tmp/pr.db \
  --oak-sources cmd,pkg,misc \
  --oak-glob '*.go' \
  --packages ./...
```

Multi-repo (one SQLite file with one PR row per repo):

```bash
GOCACHE=/tmp/go-build-cache go run ./cmd/oakgitdb build \
  --repo /path/to/geppetto \
  --repo /path/to/pinocchio \
  --base origin/main \
  --head HEAD \
  --out /tmp/multi-pr.db \
  --oak-sources cmd,pkg,misc \
  --oak-glob '*.go' \
  --packages ./...
```

Then:

```bash
sqlite3 -readonly /tmp/pr.db ".tables"
sqlite3 -readonly /tmp/pr.db "select change_type,count(*) from pr_file group by change_type;"
```

## Docs

- `docs/usage.md`
- `docs/implementation.md`
- `docs/design.md`

## Workspace type-check (geppetto + pinocchio)

In a workspace where `geppetto/` and `pinocchio/` are siblings of this repo, run:

```bash
bash scripts/typecheck-geppetto-pinocchio.sh
```

If you only want compilation/type-checking and want to reduce noise:

```bash
VET=off bash scripts/typecheck-geppetto-pinocchio.sh
```

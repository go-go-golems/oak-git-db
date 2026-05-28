.PHONY: logcopter-generate
logcopter-generate:
	GOWORK=off go generate ./...

.PHONY: logcopter-check
logcopter-check:
	GOWORK=off go tool logcopter-gen -area-prefix go-go-golems.oak-git-db -strip-prefix github.com/go-go-golems/oak-git-db -check ./pkg/... ./cmd/...

package oakgitdb

import (
	"os"
	"strings"

	"github.com/pkg/errors"
)

func readModulePath(goModPath string) (string, error) {
	b, err := os.ReadFile(goModPath)
	if err != nil {
		return "", errors.Wrap(err, "read go.mod")
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", errors.Errorf("module path not found in %s", goModPath)
}


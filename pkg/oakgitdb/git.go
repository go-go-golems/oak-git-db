package oakgitdb

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

func gitBytes(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "git %s (stderr=%s)", strings.Join(args, " "), truncate(stderr.String(), 4000))
	}
	return stdout.Bytes(), nil
}

func gitString(ctx context.Context, repoDir string, args ...string) (string, error) {
	b, err := gitBytes(ctx, repoDir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func unixToRFC3339(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func extractGitTree(ctx context.Context, repoDir string, sha string, outDir string) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha)
	cmd.Dir = repoDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "git archive stdout pipe")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "start git archive")
	}

	tr := tar.NewReader(stdout)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "read tar")
		}
		if hdr == nil {
			continue
		}

		target := filepath.Join(outDir, filepath.FromSlash(hdr.Name))
		// Prevent directory traversal.
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), filepath.Clean(outDir)+string(os.PathSeparator)) {
			return errors.Errorf("tar entry escapes target dir: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return errors.Wrap(err, "mkdir")
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errors.Wrap(err, "mkdir parent")
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return errors.Wrap(err, "create file")
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return errors.Wrap(err, "write file")
			}
			if err := f.Close(); err != nil {
				return errors.Wrap(err, "close file")
			}
		default:
			// Ignore symlinks and other special entries for analysis.
		}
	}

	if err := cmd.Wait(); err != nil {
		return errors.Wrapf(err, "git archive wait (stderr=%s)", truncate(stderr.String(), 4000))
	}
	return nil
}

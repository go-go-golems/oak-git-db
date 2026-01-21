package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-go-golems/oak-git-db/pkg/oakgitdb"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var opts oakgitdb.BuildOptions

	cmd := &cobra.Command{
		Use:   "oakgitdb",
		Short: "Build a PR-focused sqlite DB from git + oak + Go analysis",
	}

	buildCmd := &cobra.Command{
		Use:   "build",
		Short: "Build a sqlite database for PR vs origin/main",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.RepoDirs = splitCSV(opts.RepoDirs)
			opts.OakSources = splitCSV(opts.OakSources)
			opts.OakGlob = splitCSV(opts.OakGlob)
			opts.GoPackages = splitCSV(opts.GoPackages)

			if opts.OutPath == "" {
				return errors.New("--out is required")
			}
			return oakgitdb.Build(context.Background(), opts)
		},
	}

	buildCmd.Flags().StringSliceVar(&opts.RepoDirs, "repo", []string{"."}, "Repeatable repo root (e.g. --repo ../geppetto --repo ../pinocchio)")
	buildCmd.Flags().StringVar(&opts.BaseRef, "base", "origin/main", "Base ref (merge-base is computed against head)")
	buildCmd.Flags().StringVar(&opts.HeadRef, "head", "HEAD", "Head ref")
	buildCmd.Flags().StringVar(&opts.OutPath, "out", "", "Output sqlite db path")

	buildCmd.Flags().StringSliceVar(&opts.OakSources, "oak-sources", []string{"cmd", "pkg", "misc"}, "Comma-separated oak sources (dirs/files)")
	buildCmd.Flags().StringSliceVar(&opts.OakGlob, "oak-glob", []string{"*.go"}, "Comma-separated oak file globs")
	buildCmd.Flags().BoolVar(&opts.OakWithBody, "oak-with-body", false, "Include function bodies in oak match text")

	buildCmd.Flags().StringSliceVar(&opts.GoPackages, "packages", []string{"./..."}, "Comma-separated go/packages patterns")
	buildCmd.Flags().BoolVar(&opts.IncludeExternal, "include-external", false, "Include symbols outside the current module")

	cmd.AddCommand(buildCmd)
	return cmd
}

func splitCSV(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

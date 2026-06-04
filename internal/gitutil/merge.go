package gitutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// MergeImplementBranches clones repoURL into a temp dir, sequentially merges
// each branch into baseBranch with --no-ff, pushes the result, and deletes the
// merged branches from the remote. Branches that conflict are collected in
// failed; the function continues past them. Returns nil error even when some
// branches fail to merge.
func MergeImplementBranches(
	ctx context.Context,
	repoURL, baseBranch, token string,
	branches []string,
) (merged, failed []string, err error) {
	if len(branches) == 0 {
		return nil, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	tmpDir, err := os.MkdirTemp("", "rubbish-merge-*")
	if err != nil {
		return nil, nil, fmt.Errorf("merge workspace: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	authURL := injectToken(repoURL, token)

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
		return nil
	}

	if err := run("clone", authURL, "."); err != nil {
		return nil, nil, err
	}
	if err := run("config", "user.email", "rubbish-merge@localhost"); err != nil {
		return nil, nil, err
	}
	if err := run("config", "user.name", "Rubbish Merge"); err != nil {
		return nil, nil, err
	}
	if err := run("checkout", baseBranch); err != nil {
		return nil, nil, err
	}

	for _, branch := range branches {
		if ctx.Err() != nil {
			break
		}
		if fetchErr := run("fetch", authURL, branch+":"+branch); fetchErr != nil {
			failed = append(failed, branch)
			continue
		}
		if mergeErr := run("merge", "--no-ff", branch); mergeErr != nil {
			_ = run("merge", "--abort")
			failed = append(failed, branch)
			continue
		}
		merged = append(merged, branch)
	}

	if len(merged) > 0 {
		if pushErr := run("push", authURL, baseBranch); pushErr != nil {
			return merged, failed, fmt.Errorf("push %s: %w", baseBranch, pushErr)
		}
		for _, branch := range merged {
			// Non-fatal: best-effort branch cleanup.
			_ = run("push", authURL, "--delete", branch)
		}
	}

	return merged, failed, nil
}

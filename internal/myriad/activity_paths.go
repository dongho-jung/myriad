package myriad

import (
	"path/filepath"
	"slices"
	"strings"
)

func boundedActivityPaths(paths []string) ([]string, bool) {
	slices.Sort(paths)
	paths = slices.Compact(paths)
	result, size := []string{}, 0
	for _, path := range paths {
		if path == "" || path == MemoryName || strings.HasPrefix(path, MemoryName+"/") {
			continue
		}
		if len(result) == maxActivityPaths || size+len(path) > maxActivityPathBytes {
			return result, true
		}
		result = append(result, path)
		size += len(path)
	}
	return result, false
}

func activityChangedPaths(task Record) ([]string, bool, error) {
	worktree := stringValue(task, "worktree_path")
	base := stringValue(task, "base_sha")
	if base == "" {
		return nil, false, fail("activity task has no base commit")
	}
	// NUL records preserve spaces, newlines and non-ASCII names. No optional
	// index writes, external diff drivers, content output, or rename heuristics.
	changed, err := gitCommand(worktree, true, "--no-optional-locks", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, false, err
	}
	// A partial staging operation can leave the worktree equal to the base
	// while the index still contains a change that the agent will commit.
	staged, err := gitCommand(worktree, true, "--no-optional-locks", "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, false, err
	}
	untracked, err := gitCommand(worktree, true, "--no-optional-locks", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, false, err
	}
	paths, truncated := boundedActivityPaths(strings.Split(changed.Stdout+staged.Stdout+untracked.Stdout, "\x00"))
	return paths, truncated, nil
}

func normalizeActivityPaths(worktree string, paths []string) ([]string, error) {
	result := []string{}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
			return nil, fail("activity path must be a file or directory")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(worktree, path)
		}
		relative, err := filepath.Rel(worktree, filepath.Clean(path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fail("activity path is outside the managed repository: %q", path)
		}
		result = append(result, filepath.ToSlash(relative))
	}
	result, truncated := boundedActivityPaths(result)
	if truncated {
		return nil, fail("activity scope is too large; use directory paths")
	}
	return result, nil
}

func activityScope(activity Record) []string {
	return append(activityStrings(activity, "intent_paths"), activityStrings(activity, "changed_paths")...)
}

func activityOverlap(own, peer Record) ([]string, bool) {
	result := []string{}
	for _, a := range activityScope(own) {
		for _, b := range activityScope(peer) {
			switch {
			case a == b, a == ".", strings.HasPrefix(b, a+"/"):
				result = append(result, b)
			case b == ".", strings.HasPrefix(a, b+"/"):
				result = append(result, a)
			}
		}
	}
	return boundedActivityPaths(result)
}

package myriad

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func gitCommand(cwd string, check bool, args ...string) (commandResult, error) {
	argv := append([]string{"git"}, args...)
	if check {
		return checkedCommand(cwd, argv...)
	}
	return runCommand(cwd, true, argv...)
}

func repoRoot(cwd string) (string, error) {
	result, err := gitCommand(cwd, true, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return canonical(strings.TrimSpace(result.Stdout))
}

func gitCommonDir(cwd string) (string, error) {
	result, err := gitCommand(cwd, true, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return canonical(strings.TrimSpace(result.Stdout))
}

func gitRef(cwd, name string) (string, error) {
	result, err := gitCommand(cwd, true, "rev-parse", "--verify", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func branchExists(repository, branch string) bool {
	result, err := gitCommand(repository, false, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil && result.ExitCode == 0
}

func isAncestor(repository, older, newer string) bool {
	result, err := gitCommand(repository, false, "merge-base", "--is-ancestor", older, newer)
	return err == nil && result.ExitCode == 0
}

func repoKey(repository string) (string, error) {
	base := strings.ToLower(filepath.Base(repository))
	nonAlpha := regexp.MustCompile(`[^a-z0-9]+`)
	slug := strings.Trim(nonAlpha.ReplaceAllString(base, "-"), "-")
	if slug == "" {
		slug = "repository"
	}
	common, err := gitCommonDir(repository)
	if err != nil {
		return "", err
	}
	digest := sha256Hex([]byte(common))
	return fmt.Sprintf("%s-%s", slug, digest[:8]), nil
}

type worktreeRecord map[string]string

func listedWorktrees(repository string) ([]worktreeRecord, error) {
	result, err := gitCommand(repository, true, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	records := []worktreeRecord{}
	record := worktreeRecord{}
	lines := append(strings.Split(result.Stdout, "\n"), "")
	for _, line := range lines {
		if line == "" {
			if len(record) > 0 {
				records = append(records, record)
				record = worktreeRecord{}
			}
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		record[key] = value
	}
	return records, nil
}

func primaryWorktree(repository string) (string, error) {
	records, err := listedWorktrees(repository)
	if err != nil {
		return "", err
	}
	if len(records) > 0 && records[0]["worktree"] != "" {
		return canonical(records[0]["worktree"])
	}
	return canonical(repository)
}

func targetCheckout(repository, branch string) (string, error) {
	records, err := listedWorktrees(repository)
	if err != nil {
		return "", err
	}
	expected := "refs/heads/" + branch
	for _, record := range records {
		if record["branch"] == expected {
			return canonical(record["worktree"])
		}
	}
	return "", nil
}

func worktreeRegistered(repository, path string) bool {
	records, err := listedWorktrees(repository)
	if err != nil {
		return false
	}
	expected, _ := canonical(path)
	for _, record := range records {
		candidate, _ := canonical(record["worktree"])
		if candidate == expected {
			return true
		}
	}
	return false
}

type changes struct {
	Normal  []string
	Ignored []string
}

func worktreeChanges(path string) (changes, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return changes{}, nil
		}
		return changes{}, err
	}
	result, err := gitCommand(path, true, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return changes{}, err
	}
	var output changes
	for _, line := range strings.Split(result.Stdout, "\n") {
		if line == "" {
			continue
		}
		if len(line) >= 3 && (strings.HasPrefix(line, "??") || strings.HasPrefix(line, "!!")) && line[3:] == MemoryName {
			continue
		}
		if strings.HasPrefix(line, "!!") {
			output.Ignored = append(output.Ignored, line)
		} else {
			output.Normal = append(output.Normal, line)
		}
	}
	return output, nil
}

func inferTarget(repository, currentBranch string) string {
	if currentBranch == "develop" || currentBranch == "main" || currentBranch == "master" {
		return currentBranch
	}
	remote, _ := gitCommand(repository, false, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if remote.ExitCode == 0 {
		candidate := strings.TrimPrefix(strings.TrimSpace(remote.Stdout), "origin/")
		if candidate != "" && branchExists(repository, candidate) {
			return candidate
		}
	}
	candidates := []string{}
	for _, candidate := range []string{"develop", "main", "master"} {
		if branchExists(repository, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func availableTaskBranch(repository, slug string) string {
	if !branchExists(repository, slug) {
		return slug
	}
	for number := 2; ; number++ {
		candidate := fmt.Sprintf("%s-%d", slug, number)
		if !branchExists(repository, candidate) {
			return candidate
		}
	}
}

func commitTracksForbiddenPaths(repository, commit string) []string {
	result := []string{}
	for _, path := range forbiddenLocalPaths {
		probe, _ := gitCommand(repository, false, "cat-file", "-e", commit+":"+path)
		if probe.ExitCode == 0 {
			result = append(result, path)
		}
	}
	return result
}

func forbiddenHistory(repository, resultCommit, excludedCommit string) ([]Record, error) {
	listed, err := gitCommand(repository, false, "rev-list", resultCommit, "^"+excludedCommit)
	if err != nil || listed.ExitCode != 0 {
		return nil, fail("cannot inspect result commit history")
	}
	findings := []Record{}
	for _, commit := range strings.Fields(listed.Stdout) {
		paths := commitTracksForbiddenPaths(repository, commit)
		if len(paths) > 0 {
			values := make([]any, len(paths))
			for index, path := range paths {
				values[index] = path
			}
			findings = append(findings, Record{"commit": commit, "paths": values})
		}
	}
	return findings, nil
}

func treesDiffer(repository, older, newer string) (bool, error) {
	result, err := gitCommand(repository, false, "diff", "--quiet", older, newer)
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 && result.ExitCode != 1 {
		return false, fail("cannot compare result tree %s..%s", older, newer)
	}
	return result.ExitCode == 1, nil
}

func sortedCheckoutPaths(repository string) ([]string, error) {
	records, err := listedWorktrees(repository)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, record := range records {
		if record["worktree"] == "" {
			continue
		}
		path, _ := canonical(record["worktree"])
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

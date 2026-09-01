package myriad

import "os"

const diagnosticPathLimit = 100

func diagnosticProcess(value any) Record {
	recorded := anyRecord(value)
	if recorded == nil {
		return nil
	}
	result := cloneRecord(recorded)
	pid, ok := intValue(recorded["pid"])
	if !ok || pid <= 1 {
		result["alive"] = false
		result["identity_matches"] = false
		return result
	}
	observedStart, state := processIdentity(pid)
	result["alive"] = processAlive(recorded)
	result["identity_matches"] = observedStart != "" && observedStart == stringValue(recorded, "start")
	if observedStart != "" {
		result["observed_start"] = observedStart
	}
	if state != 0 {
		result["state"] = string([]byte{state})
	}
	return result
}

func diagnosticPaths(paths []string) ([]any, bool) {
	truncated := len(paths) > diagnosticPathLimit
	if truncated {
		paths = paths[:diagnosticPathLimit]
	}
	return stringsToAny(paths), truncated
}

func taskDiagnostic(store *Store, taskID string) (Record, error) {
	task, err := store.Load(taskID)
	if err != nil {
		return nil, err
	}
	summary := Record{}
	for _, key := range []string{
		"task_id", "status", "status_reason", "agent", "description", "created_at", "updated_at",
		"repository", "branch", "target_branch", "base_sha", "result_commit", "published_commit", "integrated_commit",
		"integration_strategy", "integration_target_relation", "worktree_path", "worktree_state",
		"integration_candidate", "prepared_replay", "resolved_replay_from_base", "resolved_replay_target",
		"validation_failure", "reconcile_error", "cleanup_warning",
		"last_integration_diagnostic", "integration_diagnostics", "last_publish_diagnostic", "publish_diagnostics",
		"validation_attempts", "lifecycle_history",
	} {
		if value, exists := task[key]; exists {
			summary[key] = value
		}
	}
	result := Record{"generated_at": now(), "task": summary}
	warnings := []any{}
	processes := Record{}
	for _, key := range []string{"process", "integration_process", "validation_process"} {
		if process := diagnosticProcess(task[key]); process != nil {
			processes[key] = process
			state := stringValue(process, "state")
			if state == "T" || state == "t" {
				warnings = append(warnings, key+" is stopped by job control")
			}
			if !boolValue(process, "alive", false) {
				warnings = append(warnings, key+" is no longer alive or its PID identity changed")
			}
		}
	}
	if len(processes) > 0 {
		result["processes"] = processes
	}
	status := stringValue(task, "status")
	if (status == StatusRunning || status == StatusIntegrating || status == StatusValidating) && len(processes) == 0 {
		warnings = append(warnings, "active lifecycle status has no recorded owner process")
	}

	repository, target := stringValue(task, "repository"), stringValue(task, "target_branch")
	base, taskResult := stringValue(task, "base_sha"), stringValue(task, "result_commit")
	publishedCommit := stringValue(task, "published_commit")
	refs := Record{"base_sha": base, "result_commit": taskResult, "published_commit": publishedCommit, "target_branch": target}
	targetSHA := ""
	if repository != "" && target != "" {
		if value, refErr := gitRef(repository, "refs/heads/"+target); refErr == nil {
			targetSHA = value
			refs["target_sha"] = value
		} else {
			refs["target_error"] = refErr.Error()
		}
	}
	if repository != "" && base != "" && targetSHA != "" {
		if relation, relationErr := targetHistoryRelation(repository, base, targetSHA); relationErr == nil {
			refs["target_relation"] = relation
		} else {
			refs["target_relation_error"] = relationErr.Error()
		}
	}
	if repository != "" && taskResult != "" && targetSHA != "" {
		if present, presentErr := isAncestorChecked(repository, taskResult, targetSHA); presentErr == nil {
			refs["result_on_target"] = present
		} else {
			refs["result_on_target_error"] = presentErr.Error()
		}
	}
	if repository != "" && publishedCommit != "" && targetSHA != "" {
		if present, presentErr := isAncestorChecked(repository, publishedCommit, targetSHA); presentErr == nil {
			refs["published_commit_on_target"] = present
		} else {
			refs["published_commit_on_target_error"] = presentErr.Error()
		}
	}
	result["refs"] = refs

	worktree := stringValue(task, "worktree_path")
	worktreeState := Record{"path": worktree, "recorded_state": taskWorktreeState(task)}
	if info, statErr := os.Stat(worktree); statErr == nil && info.IsDir() {
		worktreeState["exists"] = true
		if head, headErr := gitRef(worktree, "HEAD"); headErr == nil {
			worktreeState["head"] = head
		} else {
			worktreeState["head_error"] = headErr.Error()
		}
		if changes, changeErr := worktreeChanges(worktree); changeErr == nil {
			normal, normalTruncated := diagnosticPaths(changes.Normal)
			ignored, ignoredTruncated := diagnosticPaths(changes.Ignored)
			worktreeState["changes"] = normal
			worktreeState["ignored_changes"] = ignored
			if normalTruncated || ignoredTruncated {
				worktreeState["changes_truncated"] = true
			}
		} else {
			worktreeState["changes_error"] = changeErr.Error()
		}
		if repository != "" {
			if registered, registeredErr := worktreeRegistered(repository, worktree); registeredErr == nil {
				worktreeState["registered"] = registered
			} else {
				worktreeState["registered_error"] = registeredErr.Error()
			}
		}
	} else {
		worktreeState["exists"] = false
		if statErr != nil && !os.IsNotExist(statErr) {
			worktreeState["stat_error"] = statErr.Error()
		}
	}
	result["worktree"] = worktreeState

	if repository != "" {
		sessions := []any{}
		for _, session := range activeNotificationSessions(store, repository) {
			sessions = append(sessions, Record{
				"session_id": session["session_id"], "task_id": session["task_id"],
				"checkout": session["checkout"], "working_directory": session["working_directory"],
				"process": diagnosticProcess(session["process"]),
			})
		}
		result["active_sessions"] = sessions
	}
	if candidate := stringValue(task, "integration_candidate"); candidate != "" {
		candidateState := Record{"path": candidate}
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			candidateState["exists"] = true
			if head, headErr := gitRef(candidate, "HEAD"); headErr == nil {
				candidateState["head"] = head
			} else {
				candidateState["head_error"] = headErr.Error()
			}
		} else {
			candidateState["exists"] = false
		}
		result["integration_candidate"] = candidateState
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	return result, nil
}

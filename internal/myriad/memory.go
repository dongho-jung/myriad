package myriad

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

func validateMemory(value Record) error {
	schema, ok := intValue(value["schema_version"])
	if !ok || schema != 1 {
		return fail("%s schema_version must be 1", MemoryName)
	}
	settings := recordMap(value, "settings")
	memories := recordMap(value, "memories")
	if settings == nil || memories == nil {
		return fail("%s settings and memories must be JSON objects", MemoryName)
	}
	if target, exists := settings["integration_target"]; exists && target != nil {
		if _, ok := target.(string); !ok {
			return fail("%s settings.integration_target must be a string or null", MemoryName)
		}
	}
	for key, raw := range memories {
		if key == "" {
			return fail("%s memories must use non-empty keys", MemoryName)
		}
		memory, ok := raw.(map[string]any)
		if !ok {
			if typed, typedOK := raw.(Record); typedOK {
				memory = typed
				ok = true
			}
		}
		if !ok {
			return fail("%s memory %q must be a JSON object", MemoryName, key)
		}
		summary, ok := memory["summary"].(string)
		if !ok || summary == "" {
			return fail("%s memory %q must have a non-empty summary", MemoryName, key)
		}
	}
	return nil
}

func readMemory(path string) (Record, error) {
	var value Record
	if err := readJSON(path, maxMemoryBytes, &value); err != nil {
		return nil, fail("cannot read %s: %v", path, err)
	}
	if err := validateMemory(value); err != nil {
		return nil, err
	}
	return value, nil
}

func memoryBytes(value Record) ([]byte, error) {
	if err := validateMemory(value); err != nil {
		return nil, err
	}
	payload, err := marshalPrivate(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxMemoryBytes {
		return nil, fail("%s is too large: %d bytes (maximum %d)", MemoryName, len(payload), maxMemoryBytes)
	}
	return payload, nil
}

func writeMemory(path string, value Record) error {
	payload, err := memoryBytes(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, payload, 0o600)
}

func memoryTemplate(target string) Record {
	settings := Record{"integration_target": nil}
	if target != "" {
		settings["integration_target"] = target
	}
	return Record{
		"schema_version": 1,
		"settings":       settings,
		"memories":       Record{},
	}
}

func ensureMemory(store *Store, repository, currentBranch string) (string, Record, error) {
	root, err := primaryWorktree(repository)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(root, MemoryName)
	common, err := gitCommonDir(repository)
	if err != nil {
		return "", nil, err
	}
	lock, err := store.Lock("memory:"+common, true)
	if err != nil {
		return "", nil, err
	}
	defer lock.Unlock()
	tracked := []string{}
	for _, name := range forbiddenLocalPaths {
		probe, _ := gitCommand(root, false, "ls-files", "--error-unmatch", name)
		if probe.ExitCode == 0 {
			tracked = append(tracked, name)
		}
	}
	if len(tracked) > 0 {
		return "", nil, fail("machine-local paths are tracked in this checkout: %v", tracked)
	}
	created := false
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeMemory(path, memoryTemplate(inferTarget(repository, currentBranch))); err != nil {
			return "", nil, err
		}
		created = true
	}
	ignored, _ := gitCommand(root, false, "check-ignore", "--quiet", MemoryName)
	if ignored.ExitCode != 0 {
		if created {
			_ = os.Remove(path)
		}
		return "", nil, fail("%s is not ignored; install the global %s ignore first", path, MemoryName)
	}
	value, err := readMemory(path)
	return path, value, err
}

type missingValue struct{}

var missing = missingValue{}

func mergeMemory(base, current, proposed any, path string, overwrites *[]string) any {
	if reflect.DeepEqual(proposed, base) {
		return current
	}
	if reflect.DeepEqual(current, base) || reflect.DeepEqual(current, proposed) {
		return proposed
	}
	baseMap, baseOK := toAnyMap(base)
	currentMap, currentOK := toAnyMap(current)
	proposedMap, proposedOK := toAnyMap(proposed)
	if baseOK && currentOK && proposedOK {
		keysMap := map[string]struct{}{}
		for key := range baseMap {
			keysMap[key] = struct{}{}
		}
		for key := range currentMap {
			keysMap[key] = struct{}{}
		}
		for key := range proposedMap {
			keysMap[key] = struct{}{}
		}
		keys := make([]string, 0, len(keysMap))
		for key := range keysMap {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := Record{}
		for _, key := range keys {
			baseValue := any(missing)
			if value, ok := baseMap[key]; ok {
				baseValue = value
			}
			currentValue := any(missing)
			if value, ok := currentMap[key]; ok {
				currentValue = value
			}
			proposedValue := any(missing)
			if value, ok := proposedMap[key]; ok {
				proposedValue = value
			}
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			merged := mergeMemory(baseValue, currentValue, proposedValue, nextPath, overwrites)
			if _, absent := merged.(missingValue); !absent {
				result[key] = merged
			}
		}
		return result
	}
	if path == "" {
		path = "<root>"
	}
	*overwrites = append(*overwrites, path)
	return proposed
}

func toAnyMap(value any) (map[string]any, bool) {
	switch current := value.(type) {
	case Record:
		return current, true
	case map[string]any:
		return current, true
	default:
		return nil, false
	}
}

func stageMemory(task Record, value Record) error {
	worktree, err := requireString(task, "worktree_path")
	if err != nil {
		return err
	}
	if err := writeMemory(filepath.Join(worktree, MemoryName), value); err != nil {
		return err
	}
	task["memory_base"] = cloneRecord(value)
	task["memory_pending"] = true
	return nil
}

func archiveMemoryProposal(store *Store, task Record, reason string, raw []byte) error {
	var fingerprint any
	var preserved any
	if raw != nil {
		fingerprint = Record{"bytes": len(raw), "sha256": sha256Hex(raw)}
		random, err := randomHex(4)
		if err != nil {
			return err
		}
		path := filepath.Join(store.Proposals, stringValue(task, "task_id")+"-"+random+".invalid")
		if err := atomicWrite(path, raw, 0o600); err != nil {
			return err
		}
		preserved = path
	}
	task["memory_proposal"] = Record{
		"reason":         reason,
		"fingerprint":    fingerprint,
		"preserved_path": preserved,
		"recorded_at":    now(),
	}
	task["memory_pending"] = false
	task["memory_warning"] = reason
	delete(task, "memory_update")
	return store.Save(task)
}

func captureMemoryProposal(store *Store, task Record) error {
	if !boolValue(task, "memory_pending", false) {
		return nil
	}
	worktree := stringValue(task, "worktree_path")
	path := filepath.Join(worktree, MemoryName)
	raw, err := readRegular(path, maxMemoryBytes)
	if err != nil {
		return archiveMemoryProposal(store, task, fmt.Sprintf("cannot read %s: %v", path, err), nil)
	}
	var proposed Record
	if err := decodeJSON(raw, &proposed); err != nil {
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	if err := validateMemory(proposed); err != nil {
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	base := recordMap(task, "memory_base")
	if err := validateMemory(base); err != nil {
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	if reflect.DeepEqual(proposed, base) {
		task["memory_pending"] = false
		return store.Save(task)
	}
	task["memory_update"] = Record{"base": cloneRecord(base), "proposed": cloneRecord(proposed), "recorded_at": now()}
	task["memory_pending"] = false
	delete(task, "memory_proposal")
	delete(task, "memory_warning")
	return store.Save(task)
}

func applyMemoryUpdate(store *Store, task Record) error {
	update := recordMap(task, "memory_update")
	if update == nil {
		return nil
	}
	base := recordMap(update, "base")
	proposed := recordMap(update, "proposed")
	if err := validateMemory(base); err != nil {
		raw, _ := json.Marshal(update)
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	if err := validateMemory(proposed); err != nil {
		raw, _ := json.Marshal(update)
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	canonicalPath := stringValue(task, "memory_path")
	common := stringValue(task, "git_common_dir")
	lock, err := store.Lock("memory:"+common, true)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	current, err := readMemory(canonicalPath)
	if err != nil {
		raw, _ := memoryBytes(proposed)
		return archiveMemoryProposal(store, task, err.Error(), raw)
	}
	overwrites := []string{}
	merged, ok := mergeMemory(base, current, proposed, "", &overwrites).(Record)
	if !ok {
		return fail("invalid merged %s", MemoryName)
	}
	if err := validateMemory(merged); err != nil {
		return err
	}
	if _, err := memoryBytes(merged); err != nil {
		raw, _ := json.Marshal(proposed)
		return archiveMemoryProposal(store, task, "merged repository memory cannot be stored: "+err.Error(), raw)
	}
	if err := writeMemory(canonicalPath, merged); err != nil {
		return err
	}
	if len(overwrites) > 0 {
		values := make([]any, len(overwrites))
		for index, value := range overwrites {
			values[index] = value
		}
		task["memory_overwrites"] = Record{"fields": values, "recorded_at": now()}
	} else {
		delete(task, "memory_overwrites")
	}
	task["memory_base"] = merged
	task["memory_updated"] = true
	delete(task, "memory_update")
	delete(task, "memory_proposal")
	delete(task, "memory_warning")
	return store.Save(task)
}

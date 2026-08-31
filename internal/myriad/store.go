package myriad

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type Store struct {
	Root         string
	Tasks        string
	Worktrees    string
	Integrations string
	Scratch      string
	Locks        string
	Sessions     string
	Inboxes      string
	Controls     string
	Contexts     string
	Proposals    string
	Quarantine   string
	HookRuntimes string
}

func NewStore() (*Store, error) {
	root := os.Getenv("MYRIAD_STATE_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home directory: %w", err)
		}
		root = filepath.Join(home, ".local", "state", "myriad")
	}
	resolved, err := canonical(root)
	if err != nil {
		return nil, err
	}
	store := &Store{
		Root:         resolved,
		Tasks:        filepath.Join(resolved, "tasks"),
		Worktrees:    filepath.Join(resolved, "worktrees"),
		Integrations: filepath.Join(resolved, "integrations"),
		Scratch:      filepath.Join(resolved, "scratch"),
		Locks:        filepath.Join(resolved, "locks"),
		Sessions:     filepath.Join(resolved, "sessions"),
		Inboxes:      filepath.Join(resolved, "inboxes"),
		Controls:     filepath.Join(resolved, "controls"),
		Contexts:     filepath.Join(resolved, "contexts"),
		Proposals:    filepath.Join(resolved, "memory-proposals"),
		Quarantine:   filepath.Join(resolved, "quarantine"),
		HookRuntimes: filepath.Join(resolved, "hook-runtimes"),
	}
	for _, path := range []string{
		store.Root, store.Tasks, store.Worktrees, store.Integrations,
		store.Scratch, store.Locks, store.Sessions, store.Inboxes,
		store.Controls, store.Contexts, store.Proposals, store.Quarantine,
		store.HookRuntimes,
	} {
		if err := ensurePrivateDirectory(path); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Getuid()) {
		return fail("unsafe state directory refused: %s", path)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func validateTaskRecord(record Record, expectedID string) error {
	taskID := stringValue(record, "task_id")
	if taskID == "" {
		return fail("task registry entry has no task id")
	}
	if err := validateIdentifier(taskID, "task id in registry"); err != nil {
		return err
	}
	if expectedID != "" && taskID != expectedID {
		return fail("task registry id mismatch: expected %q, found %q", expectedID, taskID)
	}
	schema, ok := intValue(record["schema_version"])
	if !ok || schema != TaskRecordSchema {
		return fail("unsupported task registry schema: %s", describe(record["schema_version"]))
	}
	if value, exists := record["worktree_number"]; exists {
		number, valid := intValue(value)
		if !valid || number <= 0 {
			return fail("invalid worktree number: %s", describe(value))
		}
	}
	if value, exists := record["worktree_state"]; exists {
		state, valid := value.(string)
		if !valid || (state != worktreePending && state != worktreeCreating && state != worktreeReady) {
			return fail("invalid worktree state: %s", describe(value))
		}
	}
	return nil
}

func (store *Store) TaskPath(taskID string) (string, error) {
	if err := validateIdentifier(taskID, "task id"); err != nil {
		return "", err
	}
	return filepath.Join(store.Tasks, taskID+".json"), nil
}

func (store *Store) Save(record Record) error {
	record["schema_version"] = TaskRecordSchema
	record["updated_at"] = now()
	if err := validateTaskRecord(record, ""); err != nil {
		return err
	}
	payload, err := marshalPrivate(record)
	if err != nil {
		return err
	}
	if len(payload) > maxJSONBytes {
		return fail("task registry entry is too large: %s", stringValue(record, "task_id"))
	}
	path, _ := store.TaskPath(stringValue(record, "task_id"))
	return atomicWrite(path, payload, 0o600)
}

func (store *Store) Load(taskID string) (Record, error) {
	path, err := store.TaskPath(taskID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fail("unknown task: %s", taskID)
	}
	var record Record
	if err := readJSON(path, maxJSONBytes, &record); err != nil {
		return nil, fail("cannot safely read task registry entry %s: %v", path, err)
	}
	if err := validateTaskRecord(record, taskID); err != nil {
		return nil, err
	}
	return record, nil
}

func (store *Store) All(warn bool) []Record {
	entries, err := os.ReadDir(store.Tasks)
	if err != nil {
		return nil
	}
	result := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		taskID := strings.TrimSuffix(entry.Name(), ".json")
		record, loadErr := store.Load(taskID)
		if loadErr != nil {
			if warn {
				fmt.Fprintf(os.Stderr, "myriad: unreadable registry entry preserved: %s: %v\n", filepath.Join(store.Tasks, entry.Name()), loadErr)
			}
			continue
		}
		result = append(result, record)
	}
	return result
}

func lockDigest(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:])
}

func (store *Store) LockPath(name string) string {
	return filepath.Join(store.Locks, lockDigest(name)+".lock")
}

func (store *Store) CheckoutIdentity(checkout string) (string, error) {
	common, err := gitCommonDir(checkout)
	if err != nil {
		return "", err
	}
	resolved, err := canonical(checkout)
	if err != nil {
		return "", err
	}
	return common + "\x00" + resolved, nil
}

func (store *Store) CheckoutLockPath(checkout, identity string) (string, error) {
	if identity == "" {
		value, err := store.CheckoutIdentity(checkout)
		if err != nil {
			return "", err
		}
		identity = value
	}
	return store.LockPath("checkout:" + identity), nil
}

func (store *Store) CheckoutSessionPath(checkout, identity string) (string, error) {
	if identity == "" {
		value, err := store.CheckoutIdentity(checkout)
		if err != nil {
			return "", err
		}
		identity = value
	}
	return filepath.Join(store.Sessions, lockDigest(identity)+".json"), nil
}

func (store *Store) InboxPath(sessionID string) (string, error) {
	if err := validateIdentifier(sessionID, "session id"); err != nil {
		return "", err
	}
	return filepath.Join(store.Inboxes, sessionID+".json"), nil
}

func (store *Store) ControlSocketPath(sessionID string) (string, error) {
	if err := validateIdentifier(sessionID, "session id"); err != nil {
		return "", err
	}
	return filepath.Join(store.Controls, sessionID+".sock"), nil
}

func (store *Store) ContextPath(taskID string) (string, error) {
	if err := validateIdentifier(taskID, "task id"); err != nil {
		return "", err
	}
	return filepath.Join(store.Contexts, taskID+".json"), nil
}

func (store *Store) RepositoryActivityPath(repository string) (string, error) {
	common, err := gitCommonDir(repository)
	if err != nil {
		return "", err
	}
	return store.LockPath("repository-activity:" + common), nil
}

type fileLock struct {
	Name string
	File *os.File
}

func openLockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fail("cannot safely open lock file %s: %v", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, fail("unsafe lock file refused: %s", path)
	}
	if stat.Mode&0o777 != 0o600 {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func acquireFileLock(name, path string, exclusive, blocking bool) (*fileLock, error) {
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_SH
	if exclusive {
		operation = unix.LOCK_EX
	}
	if !blocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &lockBusyError{name: name}
		}
		return nil, err
	}
	return &fileLock{Name: name, File: file}, nil
}

func (lock *fileLock) Unlock() error {
	if lock == nil || lock.File == nil {
		return nil
	}
	err := unix.Flock(int(lock.File.Fd()), unix.LOCK_UN)
	closeErr := lock.File.Close()
	lock.File = nil
	if err != nil {
		return err
	}
	return closeErr
}

func (lock *fileLock) CloseWithoutUnlock() error {
	if lock == nil || lock.File == nil {
		return nil
	}
	err := lock.File.Close()
	lock.File = nil
	return err
}

func (store *Store) Lock(name string, blocking bool) (*fileLock, error) {
	return acquireFileLock(name, store.LockPath(name), true, blocking)
}

func (store *Store) RepositoryActivityLock(repository string, exclusive, blocking bool) (*fileLock, error) {
	path, err := store.RepositoryActivityPath(repository)
	if err != nil {
		return nil, err
	}
	return acquireFileLock("repository activity", path, exclusive, blocking)
}

func (store *Store) CheckoutLock(checkout, identity string, blocking bool) (*fileLock, error) {
	path, err := store.CheckoutLockPath(checkout, identity)
	if err != nil {
		return nil, err
	}
	return acquireFileLock("checkout "+checkout, path, true, blocking)
}

func lockFileBusy(path string) bool {
	lock, err := acquireFileLock("probe", path, true, false)
	if err != nil {
		return isLockBusy(err)
	}
	_ = lock.Unlock()
	return false
}

func nextWorktreeNumber(tasks []Record) int {
	maximum := len(tasks)
	for _, task := range tasks {
		if number, ok := intValue(task["worktree_number"]); ok && number > maximum {
			maximum = number
		}
	}
	return maximum + 1
}

func sortTasksNewest(tasks []Record) {
	sort.SliceStable(tasks, func(i, j int) bool {
		return stringValue(tasks[i], "created_at") > stringValue(tasks[j], "created_at")
	})
}

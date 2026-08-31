package myriad

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type sessionOptions struct {
	Agent            string
	TaskID           string
	WorkingDirectory string
	Repository       string
	Identity         string
	GitCommonDir     string
	BaseSHA          string
	SourceBranch     any
}

type checkoutReservation struct {
	Activity     *fileLock
	Checkout     *fileLock
	SessionPath  string
	SessionID    string
	HandoffTasks []string
}

func (reservation *checkoutReservation) Files() []*os.File {
	if reservation == nil || reservation.Checkout == nil || reservation.Checkout.File == nil {
		return nil
	}
	return []*os.File{reservation.Checkout.File}
}

func (reservation *checkoutReservation) CaptureHandoffTasks() []string {
	if reservation == nil || reservation.SessionPath == "" || reservation.SessionID == "" {
		return nil
	}
	var value Record
	if readJSON(reservation.SessionPath, maxJSONBytes, &value) != nil || stringValue(value, "session_id") != reservation.SessionID {
		return nil
	}
	result := []string{}
	for _, raw := range recordSlice(value, "handoff_task_ids") {
		if taskID, ok := raw.(string); ok {
			result = append(result, taskID)
		}
	}
	reservation.HandoffTasks = result
	return append([]string{}, result...)
}

func (reservation *checkoutReservation) Release(store *Store, checkout, identity string) {
	if reservation == nil {
		return
	}
	if reservation.Checkout != nil {
		_ = reservation.Checkout.Unlock()
	}
	if reservation.Activity != nil {
		_ = reservation.Activity.Unlock()
	}
	if reservation.SessionPath != "" {
		removeSessionMetadata(store, checkout, reservation.SessionID, identity)
	}
}

func checkoutSessionMetadata(store *Store, checkout, sessionID string, options sessionOptions) (Record, error) {
	owner := processRecord(os.Getpid(), "launcher", 0)
	if owner == nil {
		return nil, fail("cannot record checkout session process identity")
	}
	resolvedCheckout, _ := canonical(checkout)
	common := options.GitCommonDir
	if common == "" {
		var err error
		common, err = gitCommonDir(checkout)
		if err != nil {
			return nil, err
		}
	}
	base := options.BaseSHA
	if base == "" {
		var err error
		base, err = gitRef(checkout, "HEAD")
		if err != nil {
			return nil, err
		}
	}
	source := options.SourceBranch
	if source == nil {
		current, _ := gitCommand(checkout, true, "branch", "--show-current")
		branch := strings.TrimSpace(current.Stdout)
		if branch != "" {
			source = branch
		}
	}
	metadata := Record{
		"schema_version": SessionSchema,
		"kind":           "myriad-session",
		"session_id":     sessionID,
		"checkout":       resolvedCheckout,
		"git_common_dir": common,
		"base_sha":       base,
		"source_branch":  source,
		"process":        owner,
		"recorded_at":    now(),
	}
	if options.Agent != "" {
		metadata["agent"] = options.Agent
	}
	if options.TaskID != "" {
		metadata["task_id"] = options.TaskID
	}
	if options.WorkingDirectory != "" {
		working, _ := canonical(options.WorkingDirectory)
		metadata["working_directory"] = working
	}
	inbox, _ := store.InboxPath(sessionID)
	control, _ := store.ControlSocketPath(sessionID)
	metadata["notification_protocol"] = notificationProtocol
	metadata["inbox_path"] = inbox
	metadata["control_socket"] = control
	return metadata, nil
}

func validCheckoutSession(value Record, checkout, common string) bool {
	schema, ok := intValue(value["schema_version"])
	if !ok || schema != SessionSchema || stringValue(value, "kind") != "myriad-session" {
		return false
	}
	resolved, _ := canonical(checkout)
	if common == "" {
		common, _ = gitCommonDir(checkout)
	}
	base := stringValue(value, "base_sha")
	validSHA, _ := regexp.MatchString(`^[0-9a-f]{40,64}$`, base)
	return stringValue(value, "checkout") == resolved && stringValue(value, "git_common_dir") == common && validSHA && processAlive(value["process"])
}

func acquireCheckoutSession(store *Store, checkout string, recordSession bool, options sessionOptions) (*checkoutReservation, error) {
	repository := options.Repository
	if repository == "" {
		repository = checkout
	}
	activity, err := store.RepositoryActivityLock(repository, false, true)
	if err != nil {
		return nil, err
	}
	checkoutLock, err := store.CheckoutLock(checkout, options.Identity, false)
	if err != nil {
		_ = activity.Unlock()
		if isLockBusy(err) {
			return nil, nil
		}
		return nil, err
	}
	reservation := &checkoutReservation{Activity: activity, Checkout: checkoutLock}
	if !recordSession {
		return reservation, nil
	}
	sessionID, err := randomHex(16)
	if err != nil {
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	metadata, err := checkoutSessionMetadata(store, checkout, sessionID, options)
	if err != nil {
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	path, err := store.CheckoutSessionPath(checkout, options.Identity)
	if err != nil {
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	payload, err := marshalPrivate(metadata)
	if err != nil {
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	if len(payload) > maxJSONBytes {
		reservation.Release(store, checkout, options.Identity)
		return nil, fail("checkout session metadata is too large")
	}
	inbox, _ := emptyInbox(sessionID)
	if err := writeSessionInbox(store, inbox); err != nil {
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	if err := atomicWrite(path, payload, 0o600); err != nil {
		if inboxPath, pathErr := store.InboxPath(sessionID); pathErr == nil {
			_ = os.Remove(inboxPath)
		}
		reservation.Release(store, checkout, options.Identity)
		return nil, err
	}
	reservation.SessionPath = path
	reservation.SessionID = sessionID
	return reservation, nil
}

func updateSessionMetadata(path, sessionID string, updates Record) error {
	lock, err := acquireFileLock("session metadata", path+".lock", true, true)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	var value Record
	if err := readJSON(path, maxJSONBytes, &value); err != nil {
		return fail("cannot update checkout session metadata: %v", err)
	}
	if stringValue(value, "session_id") != sessionID {
		return fail("checkout session metadata changed unexpectedly")
	}
	for key, update := range updates {
		value[key] = update
	}
	payload, err := marshalPrivate(value)
	if err != nil {
		return err
	}
	if len(payload) > maxJSONBytes {
		return fail("checkout session metadata is too large")
	}
	return atomicWrite(path, payload, 0o600)
}

func transferCheckoutSessionOwner(path, sessionID string, pid int) error {
	owner := processRecord(pid, "lock-supervisor", 0)
	if owner == nil {
		return fail("cannot record checkout lock supervisor identity")
	}
	return updateSessionMetadata(path, sessionID, Record{
		"process":            owner,
		"supervised_at":      now(),
		"notification_ready": false,
		"notification_state": "starting",
	})
}

func removeSessionMetadata(store *Store, checkout, sessionID, identity string) {
	path, err := store.CheckoutSessionPath(checkout, identity)
	if err != nil {
		return
	}
	lock, err := acquireFileLock("session metadata", path+".lock", true, true)
	if err != nil {
		return
	}
	defer func() { _ = lock.Unlock() }()
	var value Record
	if readJSON(path, maxJSONBytes, &value) != nil || stringValue(value, "session_id") != sessionID {
		return
	}
	_ = os.Remove(path)
	if inbox, err := store.InboxPath(sessionID); err == nil {
		_ = os.Remove(inbox)
	}
	if control, err := store.ControlSocketPath(sessionID); err == nil {
		_ = os.Remove(control)
	}
}

func readActiveCheckoutSession(store *Store, checkout string) Record {
	identity, err := store.CheckoutIdentity(checkout)
	if err != nil {
		return nil
	}
	metadataPath, _ := store.CheckoutSessionPath(checkout, identity)
	lockPath, _ := store.CheckoutLockPath(checkout, identity)
	for attempt := 0; attempt < 10; attempt++ {
		var value Record
		if readJSON(metadataPath, maxJSONBytes, &value) == nil && validCheckoutSession(value, checkout, "") {
			busy, lockErr := lockFileBusy(lockPath)
			if lockErr == nil && busy {
				return value
			}
		}
		if attempt < 9 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

func pruneDeadSessionMetadata(store *Store) int {
	entries, _ := os.ReadDir(store.Sessions)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(store.Sessions, entry.Name())
		var value Record
		if readJSON(path, maxJSONBytes, &value) != nil || processAlive(value["process"]) {
			continue
		}
		sessionID := stringValue(value, "session_id")
		checkout := stringValue(value, "checkout")
		common := stringValue(value, "git_common_dir")
		if sessionID == "" || checkout == "" || common == "" {
			continue
		}
		identity := common + "\x00" + checkout
		expected, _ := store.CheckoutSessionPath(checkout, identity)
		lockPath, _ := store.CheckoutLockPath(checkout, identity)
		busy, lockErr := lockFileBusy(lockPath)
		if expected != path || lockErr != nil || busy {
			continue
		}
		removeSessionMetadata(store, checkout, sessionID, identity)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			removed++
		}
	}
	return removed
}

func currentAgentSession(store *Store, selected string) (string, string, Record, error) {
	sessionID := os.Getenv(envAgentSessionID)
	sessionPath := os.Getenv(envAgentSessionPath)
	if selected != "" {
		sessionID = selected
		entries, _ := os.ReadDir(store.Sessions)
		sessionPath = ""
		for _, entry := range entries {
			path := filepath.Join(store.Sessions, entry.Name())
			var value Record
			if readJSON(path, maxJSONBytes, &value) == nil && stringValue(value, "session_id") == selected {
				sessionPath = path
				break
			}
		}
	}
	if sessionID == "" || sessionPath == "" {
		return "", "", nil, fail("no active Myriad session")
	}
	var value Record
	if err := readJSON(sessionPath, maxJSONBytes, &value); err != nil {
		return "", "", nil, err
	}
	if stringValue(value, "session_id") != sessionID {
		return "", "", nil, fail("current session metadata no longer matches this process")
	}
	if selected == "" {
		inheritedPath, _ := canonical(os.Getenv(envAgentSessionPath))
		actualPath, _ := canonical(sessionPath)
		if inheritedPath != actualPath {
			return "", "", nil, fail("current session requires its inherited identity")
		}
	}
	if !processAlive(value["process"]) {
		return "", "", nil, fail("session supervisor is no longer active")
	}
	return sessionID, sessionPath, value, nil
}

func activeNotificationSessions(store *Store, repository string) []Record {
	common, err := gitCommonDir(repository)
	if err != nil {
		return nil
	}
	entries, _ := os.ReadDir(store.Sessions)
	result := []Record{}
	for _, entry := range entries {
		path := filepath.Join(store.Sessions, entry.Name())
		var value Record
		if readJSON(path, maxJSONBytes, &value) != nil {
			continue
		}
		protocol, _ := intValue(value["notification_protocol"])
		process := recordMap(value, "process")
		if protocol != notificationProtocol || stringValue(value, "git_common_dir") != common || process == nil || stringValue(process, "role") != "lock-supervisor" {
			continue
		}
		state := stringValue(value, "notification_state")
		if state != "starting" && state != "ready" {
			continue
		}
		checkout := stringValue(value, "checkout")
		identity := common + "\x00" + checkout
		expected, _ := store.CheckoutSessionPath(checkout, identity)
		lockPath, _ := store.CheckoutLockPath(checkout, identity)
		if expected == path && validCheckoutSession(value, checkout, common) {
			busy, lockErr := lockFileBusy(lockPath)
			if lockErr == nil && busy {
				result = append(result, value)
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return stringValue(result[i], "recorded_at") < stringValue(result[j], "recorded_at")
	})
	return result
}

func notifyActiveSessions(store *Store, repository string, task Record) int {
	notified := 0
	for _, session := range activeNotificationSessions(store, repository) {
		if _, err := enqueueIntegrationNotice(store, session, task); err != nil {
			fmt.Fprintf(os.Stderr, "myriad: could not notify active session %s: %v\n", stringValue(session, "session_id"), err)
			continue
		}
		process := recordMap(session, "process")
		pid, _ := intValue(process["pid"])
		if stringValue(session, "notification_state") == "ready" && boolValue(session, "notification_ready", false) {
			_ = unix.Kill(pid, unix.SIGUSR1)
		}
		notified++
	}
	return notified
}

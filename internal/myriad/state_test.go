package myriad

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDecodeJSONRejectsTrailingDocument(t *testing.T) {
	var value Record
	if err := decodeJSON([]byte(`{"schema_version":1} {"extra":true}`), &value); err == nil {
		t.Fatal("trailing JSON document was accepted")
	}
}

func TestStoreRejectsLegacySchema(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(store.Tasks, "legacy.json")
	payload := []byte("{\"schema_version\":0,\"task_id\":\"legacy\"}\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("legacy"); err == nil {
		t.Fatal("legacy task schema was accepted")
	}
}

func TestReadRegularRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	link := filepath.Join(root, "link.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(link, 1024); err == nil {
		t.Fatal("symlink state file was accepted")
	}
}

func TestAtomicWriteSetsModeDespiteRestrictiveUmask(t *testing.T) {
	root := t.TempDir()
	previous := unix.Umask(0o777)
	defer unix.Umask(previous)
	path := filepath.Join(root, "private.json")
	if err := atomicWrite(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("atomic file mode = %04o, want 0600", got)
	}
}

func TestWriteMemoryRejectsOversizedPayload(t *testing.T) {
	value := memoryTemplate("main")
	recordMap(value, "memories")["oversized"] = Record{"summary": strings.Repeat("x", maxMemoryBytes)}
	path := filepath.Join(t.TempDir(), MemoryName)
	if err := writeMemory(path, value); err == nil {
		t.Fatal("oversized repository memory was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized repository memory left a file behind: %v", err)
	}
}

func TestMemoryMergeArchivesOversizedResult(t *testing.T) {
	store := testStore(t)
	base := memoryTemplate("main")
	current := cloneRecord(base)
	recordMap(current, "memories")["current"] = Record{"summary": strings.Repeat("c", 600_000)}
	proposed := cloneRecord(base)
	recordMap(proposed, "memories")["proposed"] = Record{"summary": strings.Repeat("p", 600_000)}
	canonicalPath := filepath.Join(t.TempDir(), MemoryName)
	if err := writeMemory(canonicalPath, current); err != nil {
		t.Fatal(err)
	}
	task := Record{
		"task_id": "oversized-memory-merge", "memory_path": canonicalPath,
		"memory_update": Record{"base": base, "proposed": proposed},
	}
	if err := store.Save(task); err != nil {
		t.Fatal(err)
	}
	if err := applyMemoryUpdate(store, task); err != nil {
		t.Fatal(err)
	}
	stored, err := readMemory(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	memories := recordMap(stored, "memories")
	if memories["current"] == nil || memories["proposed"] != nil {
		t.Fatal("oversized merge changed canonical repository memory")
	}
	if recordMap(task, "memory_update") != nil || stringValue(task, "memory_warning") == "" {
		t.Fatalf("oversized merge was not archived: %s", describe(task))
	}
}

func TestMemoryMergePreservesIndependentUpdates(t *testing.T) {
	base := memoryTemplate("main")
	base["memories"] = Record{"base": Record{"summary": "base"}}
	current := cloneRecord(base)
	recordMap(current, "memories")["current"] = Record{"summary": "current"}
	proposed := cloneRecord(base)
	recordMap(proposed, "memories")["proposed"] = Record{"summary": "proposed"}
	overwrites := []string{}
	merged := mergeMemory(base, current, proposed, "", &overwrites).(Record)
	memories := recordMap(merged, "memories")
	if len(memories) != 3 || len(overwrites) != 0 {
		t.Fatalf("merged = %s, overwrites = %v", describe(merged), overwrites)
	}
}

func TestSessionMetadataUpdatesAreSerialized(t *testing.T) {
	store := testStore(t)
	repository := testRepository(t)
	reservation, err := acquireCheckoutSession(store, repository, true, sessionOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release(store, repository, "")

	const updates = 24
	start := make(chan struct{})
	errors := make(chan error, updates)
	var group sync.WaitGroup
	for index := 0; index < updates; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			errors <- updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{string(rune('a' + index)): index})
		}(index)
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var metadata Record
	if err := readJSON(reservation.SessionPath, maxJSONBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < updates; index++ {
		value, ok := intValue(metadata[string(rune('a'+index))])
		if !ok || value != index {
			t.Fatalf("missing serialized update %d in %s", index, describe(metadata))
		}
	}
}

func TestSessionMetadataRejectsOversizedUpdate(t *testing.T) {
	store := testStore(t)
	repository := testRepository(t)
	reservation, err := acquireCheckoutSession(store, repository, true, sessionOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release(store, repository, "")

	if err := updateSessionMetadata(reservation.SessionPath, reservation.SessionID, Record{
		"oversized": strings.Repeat("x", maxJSONBytes),
	}); err == nil {
		t.Fatal("oversized session metadata update was accepted")
	}
	var metadata Record
	if err := readJSON(reservation.SessionPath, maxJSONBytes, &metadata); err != nil {
		t.Fatalf("existing session metadata was corrupted: %v", err)
	}
	if _, exists := metadata["oversized"]; exists {
		t.Fatal("rejected update changed session metadata")
	}
}

func TestSessionAcquisitionRejectsOversizedMetadataWithoutArtifacts(t *testing.T) {
	store := testStore(t)
	repository := testRepository(t)
	reservation, err := acquireCheckoutSession(store, repository, true, sessionOptions{
		Repository: repository,
		Agent:      strings.Repeat("x", maxJSONBytes),
	})
	if err == nil || reservation != nil {
		if reservation != nil {
			reservation.Release(store, repository, "")
		}
		t.Fatal("oversized initial session metadata was accepted")
	}
	for _, directory := range []string{store.Sessions, store.Inboxes} {
		entries, readErr := os.ReadDir(directory)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("failed session acquisition left artifacts in %s: %v", directory, entries)
		}
	}

	retry, err := acquireCheckoutSession(store, repository, false, sessionOptions{Repository: repository})
	if err != nil || retry == nil {
		t.Fatalf("failed session acquisition retained checkout locks: (%v, %v)", retry, err)
	}
	retry.Release(store, repository, "")
}

func TestLockProbeReportsUnsafePath(t *testing.T) {
	busy, err := lockFileBusy(t.TempDir())
	if err == nil || busy {
		t.Fatalf("directory lock probe = (%t, %v), want a reported error", busy, err)
	}
}

func TestTaskContextRejectsOversizedPayload(t *testing.T) {
	store := testStore(t)
	taskID := "oversized-context"
	if err := writeTaskContext(store, taskID, Record{"oversized": strings.Repeat("x", maxJSONBytes)}); err == nil {
		t.Fatal("oversized task context was accepted")
	}
	path, _ := store.ContextPath(taskID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected task context created an artifact: %v", err)
	}
}

func TestFallbackTaskSlugUsesCompleteWords(t *testing.T) {
	if got, want := fallbackTaskSlug("Implement the extraordinarilylongwordthatislongerthanfortyeightcharacters authentication refresh flow"), "implement-authentication-refresh"; got != want {
		t.Fatalf("fallback slug = %q, want %q", got, want)
	}
	if _, err := taskSlug("truncated-"); err == nil {
		t.Fatal("invalid truncated slug was accepted")
	}
}

func TestOverlayEnvironmentReplacesValues(t *testing.T) {
	got := overlayEnvironment([]string{"A=old", "B=kept", "A=duplicate"}, map[string]string{"A": "new"})
	want := []string{"B=kept", "A=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

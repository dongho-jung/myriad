package myriad

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
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

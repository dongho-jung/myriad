package myriad

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func materializeHookRuntime(store *Store) (string, error) {
	executable, err := executablePath()
	if err != nil {
		return "", err
	}
	payload, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	digest := sha256Hex(payload)
	runtimeDirectory := filepath.Join(store.HookRuntimes, digest)
	if err := ensurePrivateDirectory(runtimeDirectory); err != nil {
		return "", err
	}
	destination := filepath.Join(runtimeDirectory, "myriad")
	if existing, err := readRegular(destination, int64(len(payload))); err == nil {
		if !bytes.Equal(existing, payload) {
			return "", fail("immutable hook runtime changed unexpectedly: %s", destination)
		}
		info, err := os.Lstat(destination)
		if err != nil {
			return "", err
		}
		if info.Mode().Perm() != 0o700 {
			if err := atomicWrite(destination, payload, 0o700); err != nil {
				return "", err
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := atomicWrite(destination, payload, 0o700); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	return destination, nil
}

func hookCommand(subcommand, launcher string) string {
	values := []string{launcher, subcommand}
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}

func readHookPayload() (Record, error) {
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxHookInputBytes {
		return nil, fail("hook input is too large")
	}
	value := Record{}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return value, nil
	}
	if err := decodeJSON(payload, &value); err != nil {
		return nil, fail("cannot read hook input: %v", err)
	}
	return value, nil
}

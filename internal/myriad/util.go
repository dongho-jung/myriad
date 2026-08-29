package myriad

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func now() string {
	return time.Now().Format(time.RFC3339)
}

func randomHex(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random identifier: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validateIdentifier(value, label string) error {
	if value == "" || !identifierPattern.MatchString(value) {
		return fail("invalid %s: %q", label, value)
	}
	return nil
}

type commandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func runCommand(cwd string, capture bool, argv ...string) (commandResult, error) {
	if len(argv) == 0 {
		return commandResult{}, fail("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	if capture {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	} else {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	err := cmd.Run()
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("start %s: %w", displayCommand(argv), err)
}

func checkedCommand(cwd string, argv ...string) (commandResult, error) {
	result, err := runCommand(cwd, true, argv...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr + result.Stdout)
		if detail != "" {
			return result, fail("command failed (%d): %s\n%s", result.ExitCode, displayCommand(argv), detail)
		}
		return result, fail("command failed (%d): %s", result.ExitCode, displayCommand(argv))
	}
	return result, nil
}

func displayCommand(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, value := range argv {
		if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`;&|<>()[]{}*?!") {
			quoted = append(quoted, value)
			continue
		}
		quoted = append(quoted, strconv.Quote(value))
	}
	return strings.Join(quoted, " ")
}

func canonical(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr == nil {
			return filepath.Join(parent, filepath.Base(absolute)), nil
		}
	}
	return filepath.Clean(absolute), nil
}

func isWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func readRegular(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot safely open %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return nil, fmt.Errorf("unsafe file refused: %s", path)
	}
	if stat.Size > limit {
		return nil, fmt.Errorf("file is too large: %s", path)
	}
	reader := io.LimitReader(file, limit+1)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("file is too large: %s", path)
	}
	return payload, nil
}

func decodeJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON")
		}
		return fmt.Errorf("unexpected trailing JSON: %w", err)
	}
	return nil
}

func readJSON(path string, limit int64, destination any) error {
	payload, err := readRegular(path, limit)
	if err != nil {
		return err
	}
	return decodeJSON(payload, destination)
}

func atomicWrite(path string, payload []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return fail("unsafe destination refused: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing file %s: %w", path, err)
	}
	random, err := randomHex(6)
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.%s", filepath.Base(path), os.Getpid(), random))
	fd, err := unix.Open(temporary, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	cleanup = false
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func marshalPrivate(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func processStart(pid int) string {
	payload, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	closing := bytes.LastIndexByte(payload, ')')
	if closing < 0 || closing+2 >= len(payload) {
		return ""
	}
	fields := strings.Fields(string(payload[closing+2:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func processRecord(pid int, role string, pgid int) Record {
	var start string
	for attempt := 0; attempt < 20; attempt++ {
		start = processStart(pid)
		if start != "" {
			break
		}
		if unix.Kill(pid, 0) != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if start == "" {
		return nil
	}
	result := Record{"pid": pid, "start": start}
	if role != "" {
		result["role"] = role
	}
	if pgid > 0 {
		result["pgid"] = pgid
	}
	return result
}

func processAlive(value any) bool {
	var record Record
	switch current := value.(type) {
	case Record:
		record = current
	case map[string]any:
		record = Record(current)
	default:
		return false
	}
	pid, ok := intValue(record["pid"])
	if !ok || pid <= 1 {
		return false
	}
	expected, ok := record["start"].(string)
	if !ok || expected == "" || unix.Kill(pid, 0) != nil {
		return false
	}
	return processStart(pid) == expected
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate myriad executable: %w", err)
	}
	return filepath.EvalSymlinks(path)
}

func platformSupported() error {
	if runtime.GOOS != "linux" {
		return fail("Myriad process supervision currently requires Linux")
	}
	return nil
}

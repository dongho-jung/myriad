package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "detach":
		child := exec.Command(os.Args[0], "child", os.Args[2])
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Args[2], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(4)
		}
		_ = child.Process.Release()
	case "child":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		<-signals
	case "wait":
		if len(os.Args) < 4 {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Args[2], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(5)
		}
		for {
			if _, err := os.Stat(os.Args[3]); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	case "commit":
		path := os.Args[2]
		if err := os.WriteFile(path, []byte("committed by agent\n"), 0o600); err != nil {
			os.Exit(11)
		}
		for _, arguments := range [][]string{
			{"add", "--", path},
			{"commit", "-q", "-m", "feat: commit managed result"},
		} {
			if err := exec.Command("git", arguments...).Run(); err != nil {
				os.Exit(12)
			}
		}
	case "attach":
		if len(os.Args) < 5 {
			os.Exit(2)
		}
		output, err := exec.Command(os.Args[2], "attach", os.Args[3]).CombinedOutput()
		if err != nil {
			_, _ = os.Stderr.Write(output)
			os.Exit(6)
		}
		worktree := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "worktree: ") {
				worktree = strings.TrimPrefix(line, "worktree: ")
			}
		}
		if worktree == "" {
			os.Exit(7)
		}
		if err := os.WriteFile(worktree+"/secondary.txt", []byte("attached\n"), 0o600); err != nil {
			os.Exit(8)
		}
		for _, arguments := range [][]string{
			{"-C", worktree, "add", "secondary.txt"},
			{"-C", worktree, "commit", "-q", "-m", "feat: update secondary repository"},
		} {
			if err := exec.Command("git", arguments...).Run(); err != nil {
				os.Exit(9)
			}
		}
		if err := os.WriteFile(os.Args[4], []byte(worktree), 0o600); err != nil {
			os.Exit(10)
		}
	default:
		os.Exit(2)
	}
}

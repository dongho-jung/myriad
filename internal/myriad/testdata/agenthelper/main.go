package main

import (
	"io"
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
	case "commit", "commit-fail", "commit-wait", "commit-publish", "commit-publish-wait":
		if os.Args[1] == "commit-wait" && len(os.Args) < 5 {
			os.Exit(2)
		}
		if os.Args[1] == "commit-publish" && len(os.Args) < 4 {
			os.Exit(2)
		}
		if os.Args[1] == "commit-publish-wait" && len(os.Args) < 6 {
			os.Exit(2)
		}
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
		if os.Args[1] == "commit-fail" {
			os.Exit(13)
		}
		if os.Args[1] == "commit-publish" {
			output, err := exec.Command(os.Args[3], "publish").CombinedOutput()
			_, _ = os.Stdout.Write(output)
			if err != nil {
				os.Exit(19)
			}
			return
		}
		if os.Args[1] == "commit-publish-wait" {
			if err := os.WriteFile(os.Args[3], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				os.Exit(20)
			}
			for {
				if _, err := os.Stat(os.Args[4]); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			output, err := exec.Command(os.Args[5], "publish").CombinedOutput()
			_, _ = os.Stdout.Write(output)
			if err != nil {
				os.Exit(21)
			}
			return
		}
		if os.Args[1] == "commit-wait" {
			if err := os.WriteFile(os.Args[3], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				os.Exit(17)
			}
			for {
				if _, err := os.Stat(os.Args[4]); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	case "noop":
		return
	case "stdin-eof":
		buffer := make([]byte, 1)
		count, err := os.Stdin.Read(buffer)
		if count != 0 || err != io.EOF {
			os.Exit(18)
		}
		_, _ = os.Stdout.WriteString("validation stdin is noninteractive\n")
	case "interrupt-hook":
		if len(os.Args) < 4 {
			os.Exit(2)
		}
		signals := make(chan os.Signal, 2)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(signals)
		stdinClosed := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(stdinClosed)
		}()
		if err := os.WriteFile(os.Args[2], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(14)
		}
		<-signals
		hook := exec.Command(os.Args[0], "hook-child", os.Args[3])
		if err := hook.Start(); err != nil {
			os.Exit(15)
		}
		_ = hook.Process.Release()
		select {
		case <-signals:
		case <-stdinClosed:
		}
	case "hook-child":
		signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
		if err := os.WriteFile(os.Args[2], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(16)
		}
		select {}
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

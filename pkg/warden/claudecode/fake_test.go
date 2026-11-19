package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeHoldDuration 是后台占位子进程的存活时长，长于测试等待 stdout EOF 的 2 s。
const fakeHoldDuration = 5 * time.Second

// TestMain 让重新执行的测试二进制充当假 Claude Code。驱动器把固定参数
// （含 --input-format）传给它；正常测试运行不带该参数。后台占位子进程用
// --fake-hold-stdout 或 --fake-hold-stdin 启动。
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--input-format":
			os.Exit(runFake())
		case "--fake-hold-stdout", "--fake-hold-stdin":
			time.Sleep(fakeHoldDuration)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// runFake 是假 Claude Code 的入口。行为由 $HOME/.fake/mode 决定。
func runFake() int {
	home := os.Getenv("HOME")
	base := filepath.Join(home, ".fake")
	modeData, err := os.ReadFile(filepath.Join(base, "mode"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: read mode:", err)
		return 2
	}
	mode := strings.TrimSpace(string(modeData))

	switch mode {
	case "silent":
		drainStdin(base)
		return 0
	case "env":
		env, err := json.Marshal(os.Environ())
		if err != nil {
			return 2
		}
		if err := os.WriteFile(filepath.Join(base, "env.json"), env, 0o600); err != nil {
			return 2
		}
		emitInit("sess-env")
		drainStdin(base)
		return 0
	case "script":
		data, err := os.ReadFile(filepath.Join(base, "stdout.jsonl"))
		if err != nil {
			return 2
		}
		if _, err := os.Stdout.Write(data); err != nil {
			return 2
		}
		drainStdin(base)
		return 0
	case "stderr":
		if _, err := os.Stderr.WriteString("fake claude stderr\n"); err != nil {
			return 2
		}
		emitInit("sess-stderr")
		drainStdin(base)
		return 0
	case "exit-in-turn":
		emitInit("sess-exit")
		_ = readOneLine()
		waitForFile(filepath.Join(base, "release"))
		return 1
	case "record":
		args, err := json.Marshal(os.Args[1:])
		if err != nil {
			return 2
		}
		if err := os.WriteFile(filepath.Join(base, "args.json"), args, 0o600); err != nil {
			return 2
		}
		wd, err := os.Getwd()
		if err != nil {
			return 2
		}
		if err := os.WriteFile(filepath.Join(base, "cwd.txt"), []byte(wd), 0o600); err != nil {
			return 2
		}
		emitInit("sess-record")
		drainStdin(base)
		return 0
	case "flood":
		emitInit("sess-flood")
		waitForFile(filepath.Join(base, "flood-start"))
		line := []byte(`{"type":"user","session_id":"sess-flood"}` + "\n")
		for written := 0; written < 256<<10; written += len(line) {
			if _, err := os.Stdout.Write(line); err != nil {
				return 2
			}
		}
		drainStdin(base)
		return 0
	case "exit-after-boundary":
		emitInit("sess-exit-b")
		_ = readOneLine()
		emitMessage(map[string]any{
			"type": "assistant", "session_id": "sess-exit-b",
			"message": map[string]any{"role": "assistant", "content": []any{}},
			"uuid":    "fake-assistant",
		})
		emitMessage(map[string]any{
			"type": "result", "subtype": "success", "result": "done",
			"is_error": false, "queued_turn_count": 0, "session_id": "sess-exit-b",
			"uuid": "fake-result",
		})
		emitMessage(map[string]any{
			"type": "system", "subtype": "session_state_changed",
			"state": "idle", "session_id": "sess-exit-b", "uuid": "fake-idle",
		})
		return 0
	case "boundary-after-close":
		emitInit("sess-bac")
		_ = readOneLine()
		_, _ = io.Copy(io.Discard, os.Stdin)
		emitMessage(map[string]any{
			"type": "result", "subtype": "success", "result": "after close",
			"is_error": false, "queued_turn_count": 0, "session_id": "sess-bac",
			"uuid": "fake-result",
		})
		emitMessage(map[string]any{
			"type": "system", "subtype": "session_state_changed",
			"state": "idle", "session_id": "sess-bac", "uuid": "fake-idle",
		})
		time.Sleep(100 * time.Millisecond)
		return 0
	case "exit-at-boundary":
		emitInit("sess-exit")
		_ = readOneLine()
		emitMessage(map[string]any{
			"type": "assistant", "session_id": "sess-exit",
			"message": map[string]any{"role": "assistant", "content": []any{}},
			"uuid":    "fake-assistant",
		})
		emitMessage(map[string]any{
			"type": "result", "subtype": "success", "result": "done",
			"is_error": false, "queued_turn_count": 0, "session_id": "sess-exit",
			"uuid": "fake-result",
		})
		emitMessage(map[string]any{
			"type": "system", "subtype": "session_state_changed",
			"state": "idle", "session_id": "sess-exit", "uuid": "fake-idle",
		})
		waitForFile(filepath.Join(base, "release"))
		return 0
	case "shutdown":
		return fakeShutdown(base)
	case "hold-stdin":
		// 输出 init 之后不读 stdin，直到测试放行；用于让一批写入停在半途。
		emitInit("sess-hold")
		waitForFile(filepath.Join(base, "release"))
		drainStdin(base)
		return 0
	case "exit-hold-stdin":
		// 后台子进程持有 stdin 读端，自己退出：写入停在半途时进程已退出。
		emitInit("sess-exit-window")
		child := exec.Command(os.Args[0], "--fake-hold-stdin")
		child.Stdin = os.Stdin
		if err := child.Start(); err != nil {
			return 2
		}
		return 1
	case "inherit-stdout":
		// 后台子进程继承 stdout，自己退出：stdout 不会随之 EOF。
		emitInit("sess-inherit")
		_ = readOneLine()
		child := exec.Command(os.Args[0], "--fake-hold-stdout")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			return 2
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "fake claude: unknown mode", mode)
		return 2
	}
}

func emitInit(sessionID string) {
	emitMessage(map[string]any{
		"type": "system", "subtype": "init", "session_id": sessionID,
		"uuid": "fake-init",
	})
}

func emitMessage(message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = os.Stdout.Write(data)
}

func readOneLine() error {
	_, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return err
}

// drainStdin 把收到的 stdin 行原样记录到 $HOME/.fake/stdin.txt，直到 EOF。
func drainStdin(base string) {
	file, err := os.OpenFile(filepath.Join(base, "stdin.txt"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		file = nil
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && file != nil {
			_, _ = file.Write(line)
		}
		if err != nil {
			break
		}
	}
	if file != nil {
		_ = file.Close()
	}
}

// waitForFile 等待测试创建放行文件，最多 10 s。
func waitForFile(path string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// fakeShutdown 忽略 stdin 关闭与 SIGTERM，记录两者的时刻，直到收到 SIGKILL。
func fakeShutdown(base string) int {
	record := func(name string) {
		file, err := os.OpenFile(filepath.Join(base, "events.txt"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(file, "%s %d\n", name, time.Now().UnixNano())
		_ = file.Close()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	go func() {
		<-signals
		record("sigterm")
	}()

	emitInit("sess-shutdown")
	_, _ = io.Copy(io.Discard, os.Stdin)
	record("stdin-close")
	select {}
}

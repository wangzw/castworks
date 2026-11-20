package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangzw/castworks/internal/testutil/fakeclock"
)

// cliTestGetenv 把一张固定的环境变量表包装成 getenv。
func cliTestGetenv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// cliTestConfigPath 在临时目录下返回配置文件路径；content 非空时写入该文件，
// 为空时文件不存在，用来看配置缺省时的行为。
func cliTestConfigPath(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extsim.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return path
}

// cliTestJob 是测试中使用的作业对象。
type cliTestJob struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// cliSubmitted 是 submit 请求体的记录。
type cliSubmitted struct {
	Project string `json:"project"`
	Content string `json:"content"`
}

// cliSubmitRecorder 起一个接受 POST /v1/jobs 的假服务端，记录最近一次请求体，
// 并返回一个 queued 的作业对象。
func cliSubmitRecorder(t *testing.T) (*httptest.Server, func() cliSubmitted) {
	t.Helper()
	var mu sync.Mutex
	var last cliSubmitted
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/jobs" {
			http.NotFound(w, r)
			return
		}
		var payload cliSubmitted
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		last = payload
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliTestJob{ID: "job-000000000000", Project: payload.Project, Status: "queued"})
	}))
	t.Cleanup(server.Close)
	return server, func() cliSubmitted {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// TestCLIResolvePrecedence 校验服务地址与项目的四级取值顺序，并校验地址末尾
// 斜杠被去掉。
func TestCLIResolvePrecedence(t *testing.T) {
	t.Parallel()

	configPath := cliTestConfigPath(t, `{"url": "http://config.invalid:1", "project": "config-project"}`)
	missingPath := cliTestConfigPath(t, "")
	bothEnv := cliTestGetenv(map[string]string{
		"EXTSIM_URL":     "http://env.invalid:1",
		"EXTSIM_PROJECT": "env-project",
	})
	noEnv := cliTestGetenv(nil)

	cases := []struct {
		name   string
		inv    *cliInvocation
		getenv func(string) string
		config string
		want   cliSettings
	}{
		{
			name:   "flags win",
			inv:    &cliInvocation{server: "http://flag.invalid:1", project: "flag-project"},
			getenv: bothEnv,
			config: configPath,
			want:   cliSettings{server: "http://flag.invalid:1", project: "flag-project"},
		},
		{
			name:   "env wins over config",
			inv:    &cliInvocation{},
			getenv: bothEnv,
			config: configPath,
			want:   cliSettings{server: "http://env.invalid:1", project: "env-project"},
		},
		{
			name:   "config wins over default",
			inv:    &cliInvocation{},
			getenv: noEnv,
			config: configPath,
			want:   cliSettings{server: "http://config.invalid:1", project: "config-project"},
		},
		{
			name:   "defaults",
			inv:    &cliInvocation{},
			getenv: noEnv,
			config: missingPath,
			want:   cliSettings{server: cliDefaultServer, project: cliDefaultProject},
		},
		{
			name:   "trailing slash trimmed",
			inv:    &cliInvocation{server: "http://flag.invalid:1/"},
			getenv: noEnv,
			config: missingPath,
			want:   cliSettings{server: "http://flag.invalid:1", project: cliDefaultProject},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCLISettings(tc.inv, tc.getenv, tc.config)
			if err != nil {
				t.Fatalf("resolveCLISettings: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveCLISettings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCLIAddressPrecedence 校验实际请求打到哪一级地址：命令行参数优先于
// 环境变量，环境变量优先于配置文件。
func TestCLIAddressPrecedence(t *testing.T) {
	t.Parallel()

	newServer := func() (*httptest.Server, func() int32) {
		var hits atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cliTestJob{ID: "job-x", Status: "completed", Conclusion: "succeeded"})
		}))
		t.Cleanup(server.Close)
		return server, hits.Load
	}

	flagServer, flagHits := newServer()
	envServer, envHits := newServer()
	configServer, configHits := newServer()

	configPath := cliTestConfigPath(t, `{"url": "`+configServer.URL+`"}`)

	run := func(args []string, env map[string]string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runJobCLI(args, strings.NewReader(""), &stdout, &stderr, cliTestGetenv(env), configPath)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
	}

	run([]string{"get", "job-x", "--server", flagServer.URL}, map[string]string{"EXTSIM_URL": envServer.URL})
	if got := flagHits(); got != 1 {
		t.Fatalf("flag server hits = %d, want 1", got)
	}
	if got := envHits(); got != 0 {
		t.Fatalf("env server hits = %d, want 0", got)
	}

	run([]string{"get", "job-x"}, map[string]string{"EXTSIM_URL": envServer.URL})
	if got := envHits(); got != 1 {
		t.Fatalf("env server hits = %d, want 1", got)
	}

	run([]string{"get", "job-x"}, nil)
	if got := configHits(); got != 1 {
		t.Fatalf("config server hits = %d, want 1", got)
	}
}

// TestCLIProjectPrecedence 校验提交时随请求发出的项目同样按四级顺序取值。
func TestCLIProjectPrecedence(t *testing.T) {
	t.Parallel()

	server, lastSubmit := cliSubmitRecorder(t)
	configPath := cliTestConfigPath(t, `{"project": "config-project"}`)
	missingPath := cliTestConfigPath(t, "")

	submit := func(args []string, env map[string]string, config string) cliSubmitted {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runJobCLI(args, strings.NewReader("content"), &stdout, &stderr, cliTestGetenv(env), config)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
		}
		return lastSubmit()
	}

	base := []string{"submit", "--server", server.URL}
	if got := submit(append(base, "--project", "flag-project"), map[string]string{"EXTSIM_PROJECT": "env-project"}, configPath); got.Project != "flag-project" {
		t.Fatalf("project with flag = %q, want flag-project", got.Project)
	}
	if got := submit(base, map[string]string{"EXTSIM_PROJECT": "env-project"}, configPath); got.Project != "env-project" {
		t.Fatalf("project with env = %q, want env-project", got.Project)
	}
	if got := submit(base, nil, configPath); got.Project != "config-project" {
		t.Fatalf("project with config = %q, want config-project", got.Project)
	}
	if got := submit(base, nil, missingPath); got.Project != cliDefaultProject {
		t.Fatalf("project with default = %q, want %q", got.Project, cliDefaultProject)
	}
}

// TestCLISubmitFromFileAndStdin 校验 submit 从文件与标准输入读取内容。
func TestCLISubmitFromFileAndStdin(t *testing.T) {
	t.Parallel()

	server, lastSubmit := cliSubmitRecorder(t)
	configPath := cliTestConfigPath(t, "")
	contentPath := filepath.Join(t.TempDir(), "content.txt")
	if err := os.WriteFile(contentPath, []byte("content from file"), 0o600); err != nil {
		t.Fatalf("write content: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"submit", "--server", server.URL, "--file", contentPath},
		strings.NewReader("standard input ignored"),
		&stdout, &stderr, cliTestGetenv(nil), configPath,
	)
	if code != 0 {
		t.Fatalf("file submit exit code = %d, stderr = %q", code, stderr.String())
	}
	if got := lastSubmit().Content; got != "content from file" {
		t.Fatalf("file submit content = %q", got)
	}
	var submitted cliTestJob
	if err := json.Unmarshal(stdout.Bytes(), &submitted); err != nil {
		t.Fatalf("decode submit output %q: %v", stdout.String(), err)
	}
	if submitted.ID == "" || submitted.Status != "queued" {
		t.Fatalf("submit output = %+v, want a queued job", submitted)
	}

	stdout.Reset()
	stderr.Reset()
	code = runJobCLI(
		[]string{"submit", "--server", server.URL},
		strings.NewReader("content from stdin"),
		&stdout, &stderr, cliTestGetenv(nil), configPath,
	)
	if code != 0 {
		t.Fatalf("stdin submit exit code = %d, stderr = %q", code, stderr.String())
	}
	if got := lastSubmit().Content; got != "content from stdin" {
		t.Fatalf("stdin submit content = %q", got)
	}
}

// TestCLISubmitServerError 校验服务端拒绝提交时退出码为 2，错误来自响应体。
func TestCLISubmitServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error": "content exceeds 64 KiB"}`)
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"submit", "--server", server.URL},
		strings.NewReader("content"), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, ""),
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if want := "server returned 400: content exceeds 64 KiB"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// TestCLIGet 校验 get 原样输出作业对象。
func TestCLIGet(t *testing.T) {
	t.Parallel()

	const body = `{"id":"job-abc","project":"demo","status":"running","conclusion":"","reason":"","created_at":"2026-01-02T03:04:05.000Z","updated_at":"2026-01-02T03:04:05.000Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-abc" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"get", "job-abc", "--server", server.URL},
		strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, ""),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), body+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// TestCLIGetNotFound 校验作业不存在时退出码为 2。
func TestCLIGetNotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error": "job not found"}`)
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"get", "job-missing", "--server", server.URL},
		strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, ""),
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if want := "server returned 404: job not found"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// TestCLIWatchExitCodes 校验 watch 的退出码与结论对应；未预期的结论按 2 处理。
func TestCLIWatchExitCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		conclusion string
		want       int
	}{
		{name: "succeeded", conclusion: "succeeded", want: 0},
		{name: "failed", conclusion: "failed", want: 1},
		{name: "unknown conclusion", conclusion: "cancelled", want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(cliTestJob{ID: "job-x", Status: "completed", Conclusion: tc.conclusion})
			}))
			t.Cleanup(server.Close)

			var stdout, stderr bytes.Buffer
			code := runJobCLI(
				[]string{"watch", "job-x", "--server", server.URL, "--interval", "1ms"},
				strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, ""),
			)
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d; stderr = %q", code, tc.want, stderr.String())
			}
			if tc.want != 2 && !strings.Contains(stdout.String(), `"id":"job-x"`) {
				t.Fatalf("stdout = %q, want the final job object", stdout.String())
			}
		})
	}
}

// TestCLIWatchServerError 校验 watch 遇到服务端错误时立即以 2 退出。
func TestCLIWatchServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"watch", "job-x", "--server", server.URL},
		strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, ""),
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestCLIWatchPollsWithFakeClock 校验 watch 在作业未完成时按 --interval 等待
// 再查询。计时器来自 fakeclock，测试推进时钟，不实际等待。
func TestCLIWatchPollsWithFakeClock(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		job := cliTestJob{ID: "job-x", Status: "running"}
		if calls.Add(1) >= 2 {
			job.Status = "completed"
			job.Conclusion = "succeeded"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(job)
	}))
	t.Cleanup(server.Close)

	clock := fakeclock.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	intervalCh := make(chan time.Duration, 1)
	configPath := cliTestConfigPath(t, "")

	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	go func() {
		done <- runJobCLIWith(cliDeps{
			args:       []string{"watch", "job-x", "--server", server.URL, "--interval", "2s"},
			stdin:      strings.NewReader(""),
			stdout:     &stdout,
			stderr:     &stderr,
			getenv:     cliTestGetenv(nil),
			configPath: configPath,
			newTimer: func(d time.Duration) <-chan time.Time {
				select {
				case intervalCh <- d:
				default:
				}
				return clock.NewTimer(d).C()
			},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clock.WaitForTimers(ctx, 1); err != nil {
		t.Fatalf("wait for watch timer: %v", err)
	}
	clock.Advance(2 * time.Second)

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("watch did not finish after the clock advanced")
	}

	if got := <-intervalCh; got != 2*time.Second {
		t.Fatalf("watch interval = %v, want 2s", got)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("job fetched %d time(s), want at least 2", got)
	}
}

// TestCLIConfigInvalid 校验配置文件无法解析时退出码为 2。
func TestCLIConfigInvalid(t *testing.T) {
	t.Parallel()

	configPath := cliTestConfigPath(t, "not json")

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"get", "job-x"},
		strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), configPath,
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "parse config") {
		t.Fatalf("stderr = %q, want a parse error", stderr.String())
	}
}

// TestCLIConfigNotReadWhenFlagsSupplyValues 校验命令行参数已给出服务地址与
// 项目时，损坏的配置文件不影响命令：不需要从配置取的值就不读配置文件。
func TestCLIConfigNotReadWhenFlagsSupplyValues(t *testing.T) {
	t.Parallel()

	server, lastSubmit := cliSubmitRecorder(t)
	configPath := cliTestConfigPath(t, "not json")

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"submit", "--server", server.URL, "--project", "flag-project"},
		strings.NewReader("content"), &stdout, &stderr, cliTestGetenv(nil), configPath,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got := lastSubmit().Project; got != "flag-project" {
		t.Fatalf("submitted project = %q, want flag-project", got)
	}
}

// TestCLIConfigNeededForMissingValue 校验 submit 需要从配置文件补足项目或
// 服务地址时，配置文件损坏则报错。get 只需要服务地址，不读配置文件，见
// TestCLIGetNeedsOnlyServer。
func TestCLIConfigNeededForMissingValue(t *testing.T) {
	t.Parallel()

	configPath := cliTestConfigPath(t, "not json")

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"submit", "--project", "demo"},
		strings.NewReader("content"), &stdout, &stderr, cliTestGetenv(nil), configPath,
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "parse config") {
		t.Fatalf("stderr = %q, want a parse error", stderr.String())
	}
}

// TestCLIGetNeedsOnlyServer 校验 get 只需要服务地址：只给 --server 时，损坏
// 的配置文件不影响查询，因为不需要从配置取项目。
func TestCLIGetNeedsOnlyServer(t *testing.T) {
	t.Parallel()

	const body = `{"id":"job-x","project":"demo","status":"completed","conclusion":"succeeded"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-x" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	code := runJobCLI(
		[]string{"get", "job-x", "--server", server.URL},
		strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), cliTestConfigPath(t, "not json"),
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), body+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// TestCLIUsageErrors 校验参数错误一律以 2 退出。
func TestCLIUsageErrors(t *testing.T) {
	t.Parallel()

	configPath := cliTestConfigPath(t, "")
	cases := [][]string{
		{},
		{"bogus"},
		{"get"},
		{"watch"},
		{"get", "a", "b"},
		{"submit", "extra"},
		{"watch", "job-x", "--interval", "0s"},
		{"watch", "job-x", "--interval", "soon"},
		{"submit", "--file", "content.txt"},
		{"get", "job-x", "--unknown", "value"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runJobCLI(args, strings.NewReader(""), &stdout, &stderr, cliTestGetenv(nil), configPath)
			if code != 2 {
				t.Fatalf("args %v: exit code = %d, want 2; stderr = %q", args, code, stderr.String())
			}
		})
	}
}

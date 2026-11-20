// 本文件实现 extsim 的作业命令行：extsim job submit|get|watch。
//
// main.go 在识别第一个参数 "job" 后调用 runJobCLI，并把 "job" 之后的参数、
// 标准输入输出、环境变量读取函数与配置文件路径一并传入；runJobCLI 返回
// 进程退出码，不调用 os.Exit。退出码：
//
//	0  成功；job watch 的结论为 succeeded
//	1  job watch 的结论为 failed
//	2  其他错误，包括参数、网络、服务端错误与未预期的状态
//
// 服务地址与项目依次取命令行参数 --server 与 --project、环境变量
// EXTSIM_URL 与 EXTSIM_PROJECT、配置文件的 url 与 project、默认值
// http://127.0.0.1:9100 与 demo。输出是服务端返回的作业对象 JSON 原文。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// cliDefaultServer 是没有其他来源时的服务地址。
	cliDefaultServer = "http://127.0.0.1:9100"
	// cliDefaultProject 是没有其他来源时的项目。
	cliDefaultProject = "demo"
	// cliDefaultInterval 是 job watch 两次查询之间的默认间隔。
	cliDefaultInterval = 2 * time.Second
	// cliRequestTimeout 是单次 HTTP 请求的上限。
	cliRequestTimeout = 30 * time.Second
	// cliMaxResponseBytes 是读取响应体时采用的上限，防止异常响应撑爆内存。
	cliMaxResponseBytes = 1 << 20
)

// cliSettings 是解析后的服务地址与项目。
type cliSettings struct {
	server  string
	project string
}

// cliConfigFile 是配置文件 /etc/extsim.json 的结构。
type cliConfigFile struct {
	URL     string `json:"url"`
	Project string `json:"project"`
}

// cliInvocation 是一次 job 子命令调用的参数。
type cliInvocation struct {
	command  string
	id       string
	file     string
	interval time.Duration
	server   string
	project  string
}

// cliDeps 是入口函数接收的全部依赖。newTimer 返回一个在 d 之后触发一次的
// 通道；测试注入 fakeclock 的计时器，避免实际等待。
type cliDeps struct {
	args       []string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	getenv     func(string) string
	configPath string
	newTimer   func(time.Duration) <-chan time.Time
}

// runJobCLI 是 "job" 子命令的入口，返回进程退出码。args 是 "job" 之后的
// 参数，第一个元素是 submit、get 或 watch。配置路径由调用方给出，生产环境
// 中 main 传入 /etc/extsim.json。
func runJobCLI(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, configPath string) int {
	return runJobCLIWith(cliDeps{
		args:       args,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		getenv:     getenv,
		configPath: configPath,
		newTimer:   cliRealTimer,
	})
}

// runJobCLIWith 与 runJobCLI 相同，但依赖全部由调用方给出，测试由此替换
// 计时器。
func runJobCLIWith(deps cliDeps) int {
	if deps.stdin == nil {
		deps.stdin = strings.NewReader("")
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.getenv == nil {
		deps.getenv = func(string) string { return "" }
	}
	if deps.newTimer == nil {
		deps.newTimer = cliRealTimer
	}

	inv, err := parseCLIArgs(deps.args)
	if err != nil {
		return cliFail(deps.stderr, err)
	}
	settings, err := resolveCLISettings(inv, deps.getenv, deps.configPath)
	if err != nil {
		return cliFail(deps.stderr, err)
	}

	runner := &cliRunner{
		deps:     deps,
		settings: settings,
		client:   &http.Client{Timeout: cliRequestTimeout},
	}
	switch inv.command {
	case "submit":
		return runner.submit(inv)
	case "get":
		return runner.get(inv.id)
	case "watch":
		return runner.watch(inv)
	default:
		// parseCLIArgs 已经拒绝未知子命令，这里只是穷举。
		return cliFail(deps.stderr, fmt.Errorf("unknown subcommand %q", inv.command))
	}
}

// parseCLIArgs 解析 "job" 之后的参数。
func parseCLIArgs(args []string) (*cliInvocation, error) {
	inv := &cliInvocation{interval: cliDefaultInterval}
	if len(args) == 0 {
		return nil, errors.New("missing subcommand; want submit, get or watch")
	}
	inv.command = args[0]
	switch inv.command {
	case "submit", "get", "watch":
	default:
		return nil, fmt.Errorf("unknown subcommand %q; want submit, get or watch", inv.command)
	}

	positional, err := parseCLIFlags(args[1:], inv)
	if err != nil {
		return nil, err
	}
	switch inv.command {
	case "submit":
		if len(positional) != 0 {
			return nil, fmt.Errorf("submit takes no positional arguments, got %d", len(positional))
		}
	case "get", "watch":
		if len(positional) != 1 {
			return nil, fmt.Errorf("%s takes one job id, got %d", inv.command, len(positional))
		}
		if positional[0] == "" {
			return nil, fmt.Errorf("%s takes a non-empty job id", inv.command)
		}
		inv.id = positional[0]
	}
	return inv, nil
}

// parseCLIFlags 扫描子命令后的参数，返回位置参数。选项可写作 --name value
// 或 --name=value，也接受单个短横线；-- 之后的全部是位置参数。
func parseCLIFlags(args []string, inv *cliInvocation) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		name := strings.TrimLeft(arg, "-")
		value := ""
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			value = name[eq+1:]
			name = name[:eq]
			hasValue = true
		}
		switch name {
		case "server", "project", "file", "interval":
		default:
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %q needs a value", arg)
			}
			i++
			value = args[i]
		}

		switch name {
		case "server":
			inv.server = value
		case "project":
			inv.project = value
		case "file":
			if inv.command != "submit" {
				return nil, errors.New("flag --file is only valid for submit")
			}
			inv.file = value
		case "interval":
			if inv.command != "watch" {
				return nil, errors.New("flag --interval is only valid for watch")
			}
			interval, err := time.ParseDuration(value)
			if err != nil {
				return nil, fmt.Errorf("invalid --interval %q: %w", value, err)
			}
			if interval <= 0 {
				return nil, fmt.Errorf("--interval must be positive, got %q", value)
			}
			inv.interval = interval
		}
	}
	return positional, nil
}

// resolveCLISettings 按命令行参数、环境变量、配置文件、默认值的顺序确定
// 服务地址与项目，前一级非空即采用。get 与 watch 按作业编号查询，只需要
// 服务地址；submit 还需要项目写进请求体。配置文件只在仍然缺少所需的值时
// 才读取，此时读取或解析失败才报错。
func resolveCLISettings(inv *cliInvocation, getenv func(string) string, configPath string) (cliSettings, error) {
	server := cliFirstNonEmpty(inv.server, getenv("EXTSIM_URL"))
	project := cliFirstNonEmpty(inv.project, getenv("EXTSIM_PROJECT"))

	// 只有还需要从配置文件取值时才读它。所需的值都已由命令行参数或环境
	// 变量给出时，配置文件缺失或损坏都不应影响命令。
	needProject := inv.command == "submit"
	var cfg cliConfigFile
	if server == "" || (needProject && project == "") {
		var err error
		if cfg, err = readCLIConfig(configPath); err != nil {
			return cliSettings{}, err
		}
	}

	server = cliFirstNonEmpty(server, cfg.URL, cliDefaultServer)
	project = cliFirstNonEmpty(project, cfg.Project, cliDefaultProject)

	server = strings.TrimRight(server, "/")
	parsed, err := url.Parse(server)
	if err != nil {
		return cliSettings{}, fmt.Errorf("invalid server address %q: %w", server, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return cliSettings{}, fmt.Errorf("invalid server address %q: want an http or https URL", server)
	}
	return cliSettings{server: server, project: project}, nil
}

// readCLIConfig 读取配置文件。文件不存在视为没有配置；其他读取或解析错误
// 原样返回。
func readCLIConfig(path string) (cliConfigFile, error) {
	if path == "" {
		return cliConfigFile{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cliConfigFile{}, nil
		}
		return cliConfigFile{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg cliConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cliConfigFile{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// cliFirstNonEmpty 返回第一个非空字符串，全空时返回空串。
func cliFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// cliRunner 持有一次调用的依赖与已确定的设置。
type cliRunner struct {
	deps     cliDeps
	settings cliSettings
	client   *http.Client
}

// submit 实现 job submit：从文件或标准输入读内容，提交后原样输出作业对象。
func (r *cliRunner) submit(inv *cliInvocation) int {
	content, err := r.readContent(inv.file)
	if err != nil {
		return cliFail(r.deps.stderr, err)
	}
	body, err := json.Marshal(struct {
		Project string `json:"project"`
		Content string `json:"content"`
	}{Project: r.settings.project, Content: content})
	if err != nil {
		return cliFail(r.deps.stderr, fmt.Errorf("encode request: %w", err))
	}

	req, err := http.NewRequest(http.MethodPost, r.jobURL(""), bytes.NewReader(body))
	if err != nil {
		return cliFail(r.deps.stderr, fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return cliFail(r.deps.stderr, fmt.Errorf("submit job: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := readCLIResponse(resp.Body)
	if err != nil {
		return cliFail(r.deps.stderr, fmt.Errorf("read submit response: %w", err))
	}
	if resp.StatusCode != http.StatusCreated {
		return cliFail(r.deps.stderr, cliServerError(resp.StatusCode, data))
	}
	if err := cliWriteJob(r.deps.stdout, data); err != nil {
		return cliFail(r.deps.stderr, err)
	}
	return 0
}

// get 实现 job get：查询一次作业并原样输出作业对象。
func (r *cliRunner) get(id string) int {
	data, status, err := r.fetch(id)
	if err != nil {
		return cliFail(r.deps.stderr, err)
	}
	if status != http.StatusOK {
		return cliFail(r.deps.stderr, cliServerError(status, data))
	}
	if err := cliWriteJob(r.deps.stdout, data); err != nil {
		return cliFail(r.deps.stderr, err)
	}
	return 0
}

// watch 实现 job watch：按间隔查询，直到作业完成，输出最终的作业对象。
// 结论 succeeded 返回 0，failed 返回 1，其他情况返回 2。
func (r *cliRunner) watch(inv *cliInvocation) int {
	for {
		data, status, err := r.fetch(inv.id)
		if err != nil {
			return cliFail(r.deps.stderr, err)
		}
		if status != http.StatusOK {
			return cliFail(r.deps.stderr, cliServerError(status, data))
		}

		var job cliJobStatus
		if err := json.Unmarshal(data, &job); err != nil {
			return cliFail(r.deps.stderr, fmt.Errorf("decode job %s: %w", inv.id, err))
		}
		switch job.Status {
		case "queued", "running":
			<-r.deps.newTimer(inv.interval)
		case "completed":
			if err := cliWriteJob(r.deps.stdout, data); err != nil {
				return cliFail(r.deps.stderr, err)
			}
			switch job.Conclusion {
			case "succeeded":
				return 0
			case "failed":
				return 1
			default:
				return cliFail(r.deps.stderr, fmt.Errorf("job %s completed with unexpected conclusion %q", inv.id, job.Conclusion))
			}
		default:
			return cliFail(r.deps.stderr, fmt.Errorf("job %s has unexpected status %q", inv.id, job.Status))
		}
	}
}

// readContent 返回作业内容：file 非空时读文件，否则读标准输入。
func (r *cliRunner) readContent(file string) (string, error) {
	if file == "" {
		data, err := io.ReadAll(r.deps.stdin)
		if err != nil {
			return "", fmt.Errorf("read standard input: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read content file %s: %w", file, err)
	}
	return string(data), nil
}

// fetch 查询一次作业，返回响应体与状态码。
func (r *cliRunner) fetch(id string) ([]byte, int, error) {
	resp, err := r.client.Get(r.jobURL(id))
	if err != nil {
		return nil, 0, fmt.Errorf("get job %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := readCLIResponse(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read job %s response: %w", id, err)
	}
	return data, resp.StatusCode, nil
}

// jobURL 拼出作业接口的地址；id 为空时指向集合。
func (r *cliRunner) jobURL(id string) string {
	target := r.settings.server + "/v1/jobs"
	if id != "" {
		target += "/" + url.PathEscape(id)
	}
	return target
}

// cliJobStatus 是 job watch 需要的作业字段。
type cliJobStatus struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// cliRealTimer 是生产环境使用的计时器。
func cliRealTimer(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// readCLIResponse 读取响应体，长度受 cliMaxResponseBytes 限制。
func readCLIResponse(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, cliMaxResponseBytes))
}

// cliServerError 把非 2xx 响应转成错误，优先采用响应体里的 error 字段。
func cliServerError(status int, body []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("server returned %d: %s", status, payload.Error)
	}
	if message := strings.TrimSpace(string(body)); message != "" {
		return fmt.Errorf("server returned %d: %s", status, message)
	}
	return fmt.Errorf("server returned %d", status)
}

// cliWriteJob 把作业对象原样写到标准输出，并保证以换行结尾。
func cliWriteJob(stdout io.Writer, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if _, err := stdout.Write(data); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if data[len(data)-1] != '\n' {
		if _, err := io.WriteString(stdout, "\n"); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	return nil
}

// cliFail 把一行 "extsim: ..." 写到标准错误并返回退出码 2。
func cliFail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "extsim: %v\n", err)
	return 2
}

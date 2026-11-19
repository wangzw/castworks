//go:build acceptance

package acceptance

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hostwatch：本地模式验收期间观察 Claude Code 访问了哪些主机。
//
// 做法是把 --claude-bin 指向一个包装脚本，脚本用 strace 跟踪
// connect、sendto、sendmsg、sendmmsg 后执行真实的 Claude Code。测试从
// connect 取远端地址与端口，从发往 53 端口的 UDP 报文里取查询的主机名，
// 去掉回环与控制面地址，得到一份“主机名 + 端口”的清单。原始 strace 文件
// 只留在工作目录里，不入库。

const hostwatchScriptName = "claude-hostwatch"

// hostwatchObservation 是一次主机观察的结果。Endpoints 是去重后的
// "地址:端口"，DNSNames 是去重后的查询主机名。
type hostwatchObservation struct {
	Endpoints []string
	DNSNames  []string
}

// writeHostwatchWrapper 在工作目录里生成包装脚本，返回它的路径。脚本以
// strace 执行真实的 Claude Code；strace 可执行文件与 trace 文件都用绝对
// 路径，因为看守进程只把注入的 PATH 传给 Claude Code，脚本自己不该依赖
// 调用者的 PATH。
func writeHostwatchWrapper(t *testing.T, workDir, claudeBin, tracePath string) string {
	t.Helper()

	straceBin, err := exec.LookPath("strace")
	if err != nil {
		t.Fatalf("主机观察需要 strace：%v", err)
	}
	if info, err := os.Stat(claudeBin); err != nil {
		t.Fatalf("Claude Code 可执行文件不可用：%v", err)
	} else if info.Mode()&0o111 == 0 {
		t.Fatalf("Claude Code 可执行文件 %s 没有执行权限", claudeBin)
	}

	// 原始 strace 文件可能含访问路径与地址，预先以 0600 建好；包装脚本里
	// 再用 umask 077，兜住 strace 自己创建或重建文件的情形。
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("创建 strace 文件 %s：%v", tracePath, err)
	}

	script := fmt.Sprintf(`#!/bin/sh
# 由验收测试生成：在 strace 下执行真实的 Claude Code。
umask 077
exec %s -f -qq -e trace=connect,sendto,sendmsg,sendmmsg -s 512 -o %s %s "$@"
`,
		shellQuote(straceBin),
		shellQuote(tracePath),
		shellQuote(claudeBin),
	)

	path := filepath.Join(workDir, hostwatchScriptName)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写入主机观察包装脚本：%v", err)
	}
	return path
}

// shellQuote 用单引号包裹一个参数，供生成的 shell 脚本使用。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var (
	// strace 的行首前缀有两种：写到终端或 stderr 时子进程的行带
	// "[pid 1234] "，用 -f -o 写文件时每一行都以裸 pid 加空格开头，
	// 例如 "12345 connect("。两种前缀以及无前缀的行都要接受。
	// 分组编号不把 pid 算进去，各行解析按自己的正则在下面说明。
	stracePIDPrefix = `^\s*(?:(?:\[pid\s+\d+\]\s+)|\d+\s+)?`
	// stracePIDLine 取出上述 pid；没有前缀的行走 0 处理。
	stracePIDLine = regexp.MustCompile(`^\s*(?:\[pid\s+(\d+)\]|(\d+))\s+`)

	// 分组：1 为文件描述符，2 为端口，3 为按 inet_addr 打印的地址，
	// 4 为按 inet_pton 打印的地址。两者取其一。
	straceConnectIPv4 = regexp.MustCompile(stracePIDPrefix +
		`connect\((\d+),\s*\{sa_family=AF_INET,\s*sin_port=htons\((\d+)\),\s*sin_addr=(?:inet_addr\("([^"]+)"\)|inet_pton\(AF_INET,\s*"([^"]+)"\))`)
	// 分组：1 为文件描述符，2 为端口，3 为地址。
	straceConnectIPv6 = regexp.MustCompile(stracePIDPrefix +
		`connect\((\d+),\s*\{sa_family=AF_INET6,\s*sin6_port=htons\((\d+)\),(?:[^}]*?)sin6_addr=inet_pton\(AF_INET6,\s*"([^"]+)"\)`)
	// 分组：1 为调用名，2 为文件描述符。
	straceSendCall = regexp.MustCompile(stracePIDPrefix + `(sendto|sendmsg|sendmmsg)\((\d+),`)
	stracePort     = regexp.MustCompile(`sin6?_port=htons\((\d+)\)`)
)

// parseHostwatch 解析 strace 输出，返回去掉回环与控制面地址后的清单。
func parseHostwatch(trace []byte, controlPlane string) hostwatchObservation {
	connected := make(map[string]int) // "pid/fd" -> 远端端口
	endpoints := make(map[string]struct{})
	names := make(map[string]struct{})

	for _, line := range strings.Split(string(trace), "\n") {
		if line == "" {
			continue
		}
		pid := 0
		if m := stracePIDLine.FindStringSubmatch(line); m != nil {
			pidText := m[1]
			if pidText == "" {
				pidText = m[2]
			}
			pid, _ = strconv.Atoi(pidText)
		}

		if m := straceConnectIPv4.FindStringSubmatch(line); m != nil {
			fd, _ := strconv.Atoi(m[1])
			port, _ := strconv.Atoi(m[2])
			ipText := m[3]
			if ipText == "" {
				ipText = m[4]
			}
			recordHostwatchEndpoint(endpoints, connected, pid, fd, ipText, port, controlPlane)
			continue
		}
		if m := straceConnectIPv6.FindStringSubmatch(line); m != nil {
			fd, _ := strconv.Atoi(m[1])
			port, _ := strconv.Atoi(m[2])
			recordHostwatchEndpoint(endpoints, connected, pid, fd, m[3], port, controlPlane)
			continue
		}

		m := straceSendCall.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fd, _ := strconv.Atoi(m[2])
		port := 0
		if pm := stracePort.FindStringSubmatch(line); pm != nil {
			port, _ = strconv.Atoi(pm[1])
		} else {
			port = connected[fdKey(pid, fd)]
		}
		if port != 53 {
			continue
		}
		for _, quoted := range straceQuotedStrings(line) {
			if name, ok := dnsQueryName(decodeStraceString(quoted)); ok {
				names[name] = struct{}{}
			}
		}
	}

	return hostwatchObservation{
		Endpoints: sortedKeys(endpoints),
		DNSNames:  sortedKeys(names),
	}
}

// recordHostwatchEndpoint 记录一个 connect 远端地址，并在同一进程与文件
// 描述符上记下端口，供后续 sendto 判断是否是 DNS 查询。
func recordHostwatchEndpoint(endpoints map[string]struct{}, connected map[string]int, pid, fd int, ipText string, port int, controlPlane string) {
	connected[fdKey(pid, fd)] = port

	ip := net.ParseIP(ipText)
	if ip == nil {
		return
	}
	if isLocalIP(ip) || isControlPlaneEndpoint(ip, port, controlPlane) {
		return
	}
	endpoints[net.JoinHostPort(ip.String(), strconv.Itoa(port))] = struct{}{}
}

func fdKey(pid, fd int) string {
	return strconv.Itoa(pid) + "/" + strconv.Itoa(fd)
}

// isLocalIP 判断地址是否在回环或未指定范围内。IPv4 映射的 IPv6 地址先
// 还原成 IPv4 再判断。
func isLocalIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4.IsLoopback() || v4.IsUnspecified()
	}
	return ip.IsLoopback() || ip.IsUnspecified()
}

// isControlPlaneEndpoint 判断地址是否就是看守进程连接的控制面地址。
func isControlPlaneEndpoint(ip net.IP, port int, controlPlane string) bool {
	if controlPlane == "" {
		return false
	}
	host, portText, err := net.SplitHostPort(controlPlane)
	if err != nil {
		return false
	}
	controlPort, err := strconv.Atoi(portText)
	if err != nil || controlPort != port {
		return false
	}
	controlIP := net.ParseIP(host)
	return controlIP != nil && controlIP.Equal(ip)
}

// straceQuotedStrings 返回一行里全部双引号字符串的内容，反斜杠转义按
// strace 的规则保留给 decodeStraceString 处理。
func straceQuotedStrings(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(line) {
			if line[j] == '\\' {
				j += 2
				continue
			}
			if line[j] == '"' {
				break
			}
			j++
		}
		if j >= len(line) {
			break
		}
		out = append(out, line[i+1:j])
		i = j
	}
	return out
}

// decodeStraceString 还原 strace 打印的字符串：八进制转义是主要形式，
// 另有 \xNN 与常见的 C 转义。
func decodeStraceString(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+1 >= len(s) {
			out = append(out, '\\')
			break
		}
		i++
		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'v':
			out = append(out, '\v')
		case 'f':
			out = append(out, '\f')
		case 'b':
			out = append(out, '\b')
		case 'a':
			out = append(out, '\a')
		case 'x':
			if i+2 < len(s) {
				if value, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					out = append(out, byte(value))
					i += 2
					continue
				}
			}
			out = append(out, 'x')
		default:
			if s[i] >= '0' && s[i] <= '7' {
				value := int(s[i] - '0')
				digits := 1
				for digits < 3 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7' {
					i++
					value = value*8 + int(s[i]-'0')
					digits++
				}
				out = append(out, byte(value))
				continue
			}
			out = append(out, s[i])
		}
	}
	return out
}

// dnsQueryName 从 UDP 载荷里取出 DNS 查询的主机名。不是合法查询时返回
// false：报文头不足、QDCOUNT 为 0、标签越界或字符不像域名都算。
func dnsQueryName(payload []byte) (string, bool) {
	if len(payload) < 13 {
		return "", false
	}
	if binary.BigEndian.Uint16(payload[4:6]) == 0 {
		return "", false
	}
	var labels []string
	for offset := 12; offset < len(payload); {
		length := int(payload[offset])
		offset++
		if length == 0 {
			break
		}
		// 压缩指针只出现在应答里；查询的 QNAME 全是标签。
		if length > 63 || offset+length > len(payload) {
			return "", false
		}
		label := string(payload[offset : offset+length])
		offset += length
		for i := 0; i < len(label); i++ {
			c := label[i]
			if c >= 0x21 && c <= 0x7e && c != '.' {
				continue
			}
			return "", false
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return "", false
	}
	name := strings.Join(labels, ".")
	if len(name) > 253 {
		return "", false
	}
	return name, true
}

// report 返回写进工作目录的清单文本。
func (o hostwatchObservation) report() string {
	var b strings.Builder
	b.WriteString("# Claude Code 访问的主机清单\n")
	b.WriteString("# connect 端点（已去掉回环与控制面）\n")
	for _, endpoint := range o.Endpoints {
		b.WriteString(endpoint)
		b.WriteString("\n")
	}
	b.WriteString("# 53 端口 UDP 查询中出现的主机名\n")
	for _, name := range o.DNSNames {
		b.WriteString(name)
		b.WriteString("\n")
	}
	return b.String()
}

// allowed 判断清单是否只有 api.anthropic.com:443。端口来自 connect 端点，
// 主机名来自 DNS 查询；两者都对才算通过。
func (o hostwatchObservation) allowed() bool {
	if len(o.Endpoints) == 0 || len(o.DNSNames) == 0 {
		return false
	}
	for _, name := range o.DNSNames {
		if name != "api.anthropic.com" {
			return false
		}
	}
	for _, endpoint := range o.Endpoints {
		_, port, err := net.SplitHostPort(endpoint)
		if err != nil || port != "443" {
			return false
		}
	}
	return true
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// TestHostwatchParsesStrace 覆盖解析规则：IPv4 与 IPv6 连接、回环与控制面
// 地址的过滤、DNS 查询主机名的提取，以及清单的通过判定。
func TestHostwatchParsesStrace(t *testing.T) {
	trace := strings.Join([]string{
		`[pid  1234] connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("160.79.104.10")}, 16) = 0`,
		`connect(4, {sa_family=AF_INET6, sin6_port=htons(443), sin6_flowinfo=htonl(0), sin6_addr=inet_pton(AF_INET6, "2606:4700::6810:1234"), sin6_scope_id=0}, 28) = 0`,
		`connect(5, {sa_family=AF_INET, sin_port=htons(8080), sin_addr=inet_addr("127.0.0.1")}, 16) = 0`,
		`connect(6, {sa_family=AF_UNIX, sun_path="/var/run/nscd/socket"}, 110) = 0`,
		`[pid  1234] connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.53")}, 16) = 0`,
		`[pid  1234] sendto(7, "\253\315\1\0\0\1\0\0\0\0\0\0\3api\11anthropic\3com\0\0\1\0\1", 35, MSG_NOSIGNAL, NULL, 0) = 35`,
		`[pid  1234] sendmmsg(8, [{msg_hdr={msg_name={sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("10.0.0.1")}, msg_namelen=16, msg_iov=[{iov_base="\253\315\1\0\0\1\0\0\0\0\0\0\6github\3com\0\0\1\0\1", iov_len=28}], msg_iovlen=1}}], 1, MSG_NOSIGNAL) = 1`,
	}, "\n")

	obs := parseHostwatch([]byte(trace), "127.0.0.1:9000")
	wantEndpoints := []string{"160.79.104.10:443", "[2606:4700::6810:1234]:443"}
	if !equalStrings(obs.Endpoints, wantEndpoints) {
		t.Fatalf("端点为 %v，想要 %v", obs.Endpoints, wantEndpoints)
	}
	wantNames := []string{"api.anthropic.com", "github.com"}
	if !equalStrings(obs.DNSNames, wantNames) {
		t.Fatalf("主机名为 %v，想要 %v", obs.DNSNames, wantNames)
	}
	if obs.allowed() {
		t.Fatalf("清单含 github.com，不应判定通过：\n%s", obs.report())
	}

	onlyAnthropic := parseHostwatch([]byte(strings.Join([]string{
		`connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("160.79.104.10")}, 16) = 0`,
		`connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.53")}, 16) = 0`,
		`sendto(7, "\253\315\1\0\0\1\0\0\0\0\0\0\3api\11anthropic\3com\0\0\1\0\1", 35, MSG_NOSIGNAL, NULL, 0) = 35`,
	}, "\n")), "127.0.0.1:9000")
	if !onlyAnthropic.allowed() {
		t.Fatalf("只有 api.anthropic.com:443 时应判定通过：\n%s", onlyAnthropic.report())
	}

	controlPlane := parseHostwatch([]byte(
		`connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("160.79.104.10")}, 16) = 0`,
	), "160.79.104.10:443")
	if len(controlPlane.Endpoints) != 0 {
		t.Fatalf("控制面地址应被去掉，实际为 %v", controlPlane.Endpoints)
	}

	extraPort := parseHostwatch([]byte(strings.Join([]string{
		`connect(3, {sa_family=AF_INET, sin_port=htons(8443), sin_addr=inet_addr("160.79.104.10")}, 16) = 0`,
		`connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.53")}, 16) = 0`,
		`sendto(7, "\253\315\1\0\0\1\0\0\0\0\0\0\3api\11anthropic\3com\0\0\1\0\1", 35, MSG_NOSIGNAL, NULL, 0) = 35`,
	}, "\n")), "")
	if extraPort.allowed() {
		t.Fatalf("端口 8443 不应判定通过：\n%s", extraPort.report())
	}
}

// TestHostwatchParsesBarePIDPrefix covers the file strace writes with -f -o:
// every line starts with the pid itself, not with "[pid N]". The sendto line
// carries no port of its own, so it also checks that the pid parsed from the
// bare prefix is what finds the earlier connect on the same file descriptor.
func TestHostwatchParsesBarePIDPrefix(t *testing.T) {
	trace := strings.Join([]string{
		`3597398 connect(5, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("160.79.104.10")}, 16) = 0`,
		`3597398 connect(7, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.53")}, 16) = 0`,
		`3597398 sendto(7, "\253\315\1\0\0\1\0\0\0\0\0\0\3api\11anthropic\3com\0\0\1\0\1", 35, MSG_NOSIGNAL, NULL, 0) = 35`,
		`3597399 connect(8, {sa_family=AF_INET6, sin6_port=htons(443), sin6_flowinfo=htonl(0), sin6_addr=inet_pton(AF_INET6, "2606:4700::6810:1234"), sin6_scope_id=0}, 28) = 0`,
		`3597399 connect(9, {sa_family=AF_INET, sin_port=htons(8080), sin_addr=inet_addr("127.0.0.1")}, 16) = 0`,
	}, "\n")

	obs := parseHostwatch([]byte(trace), "127.0.0.1:9000")
	wantEndpoints := []string{"160.79.104.10:443", "[2606:4700::6810:1234]:443"}
	if !equalStrings(obs.Endpoints, wantEndpoints) {
		t.Fatalf("端点为 %v，想要 %v", obs.Endpoints, wantEndpoints)
	}
	wantNames := []string{"api.anthropic.com"}
	if !equalStrings(obs.DNSNames, wantNames) {
		t.Fatalf("主机名为 %v，想要 %v", obs.DNSNames, wantNames)
	}
	if !obs.allowed() {
		t.Fatalf("裸 pid 前缀的清单应判定通过：\n%s", obs.report())
	}
}

// equalStrings 比较两个字符串切片是否逐项相等。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

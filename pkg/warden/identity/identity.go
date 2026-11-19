// Package identity 从 systemInfo 卷投射的 actor-name 文件中确定运行实例的 run_id。
//
// 对外接口：
//
//	func Read(path string) (runID string, err error)
//	func Parse(actorName string) (runID string, err error)
//
// Parse 校验 actor 名：内容不去除空白，必须以 "cw-" 开头，其后是非空且不含
// 空白字符的 run_id；不满足时返回错误。Read 读取整个文件后交给 Parse。
// 进程装配在 actor 模式下调用 Read("/run/ate/actor-name")，出错时记录错误
// 并以非零状态退出。
package identity

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// Prefix 是 actor 名的前缀。
const Prefix = "cw-"

// Read 读取 path 指向的 actor-name 文件，返回其中的 run_id。
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("identity: read actor name: %w", err)
	}
	runID, err := Parse(string(data))
	if err != nil {
		return "", fmt.Errorf("identity: %s: %w", path, err)
	}
	return runID, nil
}

// Parse 校验 actor 名并返回其中的 run_id。
func Parse(actorName string) (string, error) {
	if !strings.HasPrefix(actorName, Prefix) {
		return "", fmt.Errorf("actor name %q does not start with %q", actorName, Prefix)
	}
	runID := strings.TrimPrefix(actorName, Prefix)
	if runID == "" {
		return "", errors.New("actor name has an empty run id")
	}
	if strings.IndexFunc(runID, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("actor name %q contains whitespace in the run id", actorName)
	}
	return runID, nil
}

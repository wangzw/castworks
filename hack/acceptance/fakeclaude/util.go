package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// base32Alphabet 与作业标识、订阅标识使用的小写 base32 字符集一致。
const base32Alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// randomBase32 返回 n 个小写 base32 随机字符，用于任务标识与运行标识。
func randomBase32(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = base32Alphabet[int(b[i])%len(base32Alphabet)]
	}
	return string(b)
}

// randomHex 返回 n 字节的十六进制串。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUID 返回一个随机 UUID，与 Claude Code 消息里的 uuid 同形。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newToolUseID 返回 assistant 消息中 tool_use 的标识。
func newToolUseID() string {
	return "toolu_" + randomHex(10)
}

// newTaskID 返回后台任务的标识，形如样本中的 bw18fqiul。
func newTaskID() string {
	return randomBase32(9)
}

// newRunID 返回后台任务的 run_id，形如样本中的 0mwnyh3zf-4c256146。
func newRunID() string {
	return randomBase32(9) + "-" + randomHex(4)
}

// nowMillis 返回当前时刻的 Unix 毫秒，用于 task_updated 的 end_time。
func nowMillis() int64 {
	return time.Now().UnixMilli()
}

// trimTrailingNewline 去掉字符串末尾的一个换行，保留其余内容。
func trimTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

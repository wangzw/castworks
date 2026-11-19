package claudecode

import (
	"fmt"
	"unicode/utf8"
)

// truncateOutput 把超过 1 MiB 的回合输出截断在 UTF-8 字符边界上，并追加
// 说明，追加后的总长不超过 1 MiB。
func truncateOutput(output string) string {
	if len(output) <= maxOutputBytes {
		return output
	}
	suffix := fmt.Sprintf("\n[output truncated: %d bytes]", len(output))
	keep := maxOutputBytes - len(suffix)
	if keep < 0 {
		keep = 0
	}
	cut := keep
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return output[:cut] + suffix
}

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/wangzw/castworks/pkg/runcontroller"
)

// maxCredentialFileSize is the largest LLM credential file the control plane
// accepts. Larger files are rejected rather than truncated.
const maxCredentialFileSize = 16 << 10 // 16 KiB

// readLLMCredential reads the LLM credential file at path and pairs its
// contents with kind. Trailing whitespace (any rune for which unicode.IsSpace
// is true) is removed; leading and inner whitespace is preserved.
//
// The returned error may name the path but never includes the file contents.
func readLLMCredential(path string, kind runcontroller.LLMCredentialKind) (runcontroller.LLMCredential, error) {
	f, err := os.Open(path)
	if err != nil {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: %w", path, err)
	}
	if info.IsDir() {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: is a directory", path)
	}

	// Read one byte past the limit so a file exactly at the limit succeeds
	// while anything larger is rejected without buffering the whole file.
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialFileSize+1))
	if err != nil {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: %w", path, err)
	}
	if len(data) > maxCredentialFileSize {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: larger than %d bytes", path, maxCredentialFileSize)
	}

	value := strings.TrimRightFunc(string(data), unicode.IsSpace)
	if value == "" {
		return runcontroller.LLMCredential{}, fmt.Errorf("read LLM credential file %q: empty", path)
	}

	return runcontroller.LLMCredential{Kind: kind, Value: value}, nil
}

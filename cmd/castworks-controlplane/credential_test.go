package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangzw/castworks/internal/runcontroller"
)

// credentialFakeToken is a distinctive value planted in files whose error
// paths must never echo the file contents.
const credentialFakeToken = "sk-fake-token-do-not-log-7f3a"

func writeCredentialFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llm-credential")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	return path
}

func TestCredentialTrailingWhitespaceTrimmed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "trailing newline", content: "token-value\n", want: "token-value"},
		{name: "trailing spaces and tabs", content: "token-value \t  ", want: "token-value"},
		{name: "crlf", content: "token-value\r\n", want: "token-value"},
		{name: "several trailing newlines", content: "token-value\n\n\n", want: "token-value"},
		{name: "trailing unicode space", content: "token-value  ", want: "token-value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeCredentialFile(t, tt.content)
			got, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
			if err != nil {
				t.Fatalf("readLLMCredential() error = %v", err)
			}
			if got.Value != tt.want {
				t.Errorf("Value = %q, want %q", got.Value, tt.want)
			}
			if got.Kind != runcontroller.LLMCredentialAPIKey {
				t.Errorf("Kind = %q, want %q", got.Kind, runcontroller.LLMCredentialAPIKey)
			}
		})
	}
}

func TestCredentialLeadingAndInnerWhitespacePreserved(t *testing.T) {
	t.Parallel()

	path := writeCredentialFile(t, " \t token with inner space \t\r\n")
	got, err := readLLMCredential(path, runcontroller.LLMCredentialOAuthToken)
	if err != nil {
		t.Fatalf("readLLMCredential() error = %v", err)
	}
	want := " \t token with inner space"
	if got.Value != want {
		t.Errorf("Value = %q, want %q", got.Value, want)
	}
}

func TestCredentialKindCarriedThrough(t *testing.T) {
	t.Parallel()

	for _, kind := range []runcontroller.LLMCredentialKind{
		runcontroller.LLMCredentialAPIKey,
		runcontroller.LLMCredentialOAuthToken,
	} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			path := writeCredentialFile(t, "token-value\n")
			got, err := readLLMCredential(path, kind)
			if err != nil {
				t.Fatalf("readLLMCredential() error = %v", err)
			}
			if got.Kind != kind {
				t.Errorf("Kind = %q, want %q", got.Kind, kind)
			}
		})
	}
}

func TestCredentialAtSizeLimitAccepted(t *testing.T) {
	t.Parallel()

	content := credentialFakeToken + strings.Repeat("x", maxCredentialFileSize-len(credentialFakeToken))
	if len(content) != maxCredentialFileSize {
		t.Fatalf("test content is %d bytes, want %d", len(content), maxCredentialFileSize)
	}

	path := writeCredentialFile(t, content)
	got, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
	if err != nil {
		t.Fatalf("readLLMCredential() error = %v", err)
	}
	if got.Value != content {
		t.Errorf("Value length = %d, want %d", len(got.Value), len(content))
	}
}

func TestCredentialEmptyFile(t *testing.T) {
	t.Parallel()

	// An empty file has no content, so there is nowhere to plant the fake
	// token; the error can only be checked for its presence.
	path := writeCredentialFile(t, "")
	_, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
	if err == nil {
		t.Fatal("readLLMCredential() error = nil, want error for an empty file")
	}
}

func TestCredentialWhitespaceOnlyFile(t *testing.T) {
	t.Parallel()

	// The file must stay whitespace-only to trigger the error, so it cannot
	// also carry the fake token.
	path := writeCredentialFile(t, " \t\r\n  \n")
	_, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
	if err == nil {
		t.Fatal("readLLMCredential() error = nil, want error for a whitespace-only file")
	}
}

func TestCredentialMissingFile(t *testing.T) {
	t.Parallel()

	// No file exists, so no content can be read or echoed.
	path := filepath.Join(t.TempDir(), "absent")
	_, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
	if err == nil {
		t.Fatal("readLLMCredential() error = nil, want error for a missing file")
	}
}

func TestCredentialDirectory(t *testing.T) {
	t.Parallel()

	// The path is a directory, not a file, so there is no file content to
	// plant or echo.
	path := t.TempDir()
	_, err := readLLMCredential(path, runcontroller.LLMCredentialAPIKey)
	if err == nil {
		t.Fatal("readLLMCredential() error = nil, want error for a directory")
	}
}

func TestCredentialTooLarge(t *testing.T) {
	t.Parallel()

	content := credentialFakeToken + strings.Repeat("x", maxCredentialFileSize+1-len(credentialFakeToken))
	if len(content) != maxCredentialFileSize+1 {
		t.Fatalf("test content is %d bytes, want %d", len(content), maxCredentialFileSize+1)
	}

	path := writeCredentialFile(t, content)
	_, err := readLLMCredential(path, runcontroller.LLMCredentialOAuthToken)
	if err == nil {
		t.Fatal("readLLMCredential() error = nil, want error for an oversized file")
	}
	if strings.Contains(err.Error(), credentialFakeToken) {
		t.Errorf("error text %q contains file content", err.Error())
	}
}

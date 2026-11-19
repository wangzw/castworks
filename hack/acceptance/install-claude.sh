#!/usr/bin/env bash
# install-claude.sh installs the pinned Claude Code used by the local
# acceptance run and prints the absolute path of its executable.
#
# The version matches the actor image. The developer machine's own claude may
# be older, so the acceptance run never uses whatever is on PATH.
#
# The package is installed under
# ${XDG_CACHE_HOME:-$HOME/.cache}/castworks/claude-code-<version>/, which is
# outside the repository. When that installation already reports the right
# version, nothing is downloaded.
set -euo pipefail

version="2.1.328"
cache_root="${XDG_CACHE_HOME:-$HOME/.cache}"
install_dir="$cache_root/castworks/claude-code-$version"
claude_bin="$install_dir/node_modules/.bin/claude"

installed_version() {
	"$claude_bin" --version 2>/dev/null | awk 'NR == 1 { print $1 }'
}

is_installed() {
	[ -x "$claude_bin" ] || return 1
	[ "$(installed_version)" = "$version" ]
}

if ! is_installed; then
	if ! command -v npm >/dev/null 2>&1; then
		echo "install-claude.sh: npm is required to install @anthropic-ai/claude-code@$version" >&2
		exit 1
	fi
	mkdir -p "$install_dir"
	if ! npm install --prefix "$install_dir" --no-audit --no-fund --loglevel=error \
		"@anthropic-ai/claude-code@$version" >&2; then
		echo "install-claude.sh: npm install failed for @anthropic-ai/claude-code@$version" >&2
		exit 1
	fi
fi

if ! is_installed; then
	echo "install-claude.sh: Claude Code at $claude_bin is not version $version (found: $(installed_version || true))" >&2
	exit 1
fi

readlink -f "$claude_bin"

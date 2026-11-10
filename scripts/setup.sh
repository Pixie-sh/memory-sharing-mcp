#!/usr/bin/env bash
# Guided first-time setup: choose a provider and store, write the config,
# create the store if new, install the service, then connect an agent.
#
#   scripts/setup.sh <memory-sharing-mcp binary to install>
#
# An existing config is kept as is. Without a terminal, every answer is its default.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 <memory-sharing-mcp binary>" >&2; exit 2; }
bin=$1
main=${MSM_CONFIG:-$HOME/.config/memory-sharing-mcp/config.json}
url=${URL:-http://127.0.0.1:8790}
mnemosyne_version=$("$bin" version mnemosyne)

die() { echo "error: $*" >&2; exit 1; }
json() { local s=${1//\\/\\\\}; printf '"%s"' "${s//\"/\\\"}"; }
# ask PROMPT DEFAULT prints the answer, or DEFAULT when empty or not interactive.
ask() {
	local v=
	[ -t 0 ] && read -rp "$1 [$2]: " v
	echo "${v:-$2}"
}
tilde() { echo "${1/#\~/$HOME}"; }

if [ -f "$main" ]; then
	echo "Config $main already exists; keeping it."
else
	echo "1. Provider"
	case $(ask "Memory provider: mnemosyne or mem0" mnemosyne) in
	mnemosyne) ;;
	mem0) die "the mem0 provider is not available yet; only mnemosyne works today" ;;
	*) die "unknown provider; choose mnemosyne or mem0" ;;
	esac

	echo "2. Mnemosyne runtime"
	py=$(tilde "$(ask "Python of the mnemosyne-memory $mnemosyne_version install" "$HOME/.local/share/uv/tools/mnemosyne-memory/bin/python")")
	if [ ! -x "$py" ]; then
		command -v uv >/dev/null || die "$py not found; install uv, then run: uv tool install mnemosyne-memory==$mnemosyne_version"
		[ "$(ask "$py not found. Run 'uv tool install mnemosyne-memory==$mnemosyne_version'? (y/n)" y)" = y ] || die "install mnemosyne-memory, then run setup again"
		uv tool install "mnemosyne-memory==$mnemosyne_version"
		[ -x "$py" ] || die "$py still not found; give the interpreter path of the mnemosyne-memory install"
	fi

	echo "3. Store"
	new=
	case $(ask "Create a new store or use an existing one? (new/existing)" new) in
	new)
		data=$(tilde "$(ask "Directory for the new store" "$HOME/.local/share/memory-sharing-mcp/mnemosyne")")
		new=1
		;;
	existing)
		data=$(tilde "$(ask "Directory holding mnemosyne.db (Pi uses ~/.pi/agent/mnemosyne)" "$HOME/.pi/agent/mnemosyne")")
		[ -f "$data/mnemosyne.db" ] || die "no mnemosyne.db in $data"
		echo "   Turn off every agent's direct access to this store (such as Pi's Mnemosyne extension);"
		echo "   from now on only the service may open it."
		;;
	*) die "answer new or existing" ;;
	esac

	mkdir -p "$(dirname "$main")"
	printf '{\n  "provider": "mnemosyne",\n  "mnemosyne": {\n    "data_dir": %s,\n    "python": %s\n  }\n}\n' \
		"$(json "$data")" "$(json "$py")" >"$main"
	echo "   config written to $main"
	if [ -n "$new" ]; then
		"$bin" --config "$main" store init
	fi
fi

echo "4. Service"
"$bin" --config "$main" install
for _ in $(seq 30); do
	curl -sf --max-time 2 "$url/readyz" >/dev/null && break
	sleep 1
done
curl -sf --max-time 2 "$url/readyz" >/dev/null || die "service not ready at $url after 30 s; check 'make logs'"
echo "   service ready at $url"

echo "5. Agents"
if [ -n "${AGENT:-}" ] || { [ -t 0 ] && [ "$(ask "Connect an agent now? (y/n)" y)" = y ]; }; then
	exec "$(dirname "$0")/gen-config.sh"
fi
echo "Connect agents later with 'make gen-config'."

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
	provider=$(ask "Memory provider: mnemosyne or mem0" mnemosyne)
	case $provider in
	mnemosyne)
		version=$("$bin" version mnemosyne)
		py_default=$HOME/.local/share/uv/tools/mnemosyne-memory/bin/python
		install="uv tool install mnemosyne-memory==$version"
		marker=mnemosyne.db
		existing_default=$HOME/.pi/agent/mnemosyne
		;;
	mem0)
		version=$("$bin" version mem0)
		py_default=$HOME/.local/share/memory-sharing-mcp/mem0-env/bin/python
		marker=history.db
		existing_default=$HOME/.mem0
		;;
	*) die "unknown provider; choose mnemosyne or mem0" ;;
	esac

	echo "2. Runtime"
	py=$(tilde "$(ask "Python of the $provider $version install" "$py_default")")
	# mem0ai has no command of its own, so it gets a plain environment instead of a uv tool.
	[ "$provider" = mem0 ] && install="uv venv ${py%/bin/python} && uv pip install --python $py mem0ai==$version"
	if [ ! -f "$py" ] || [ ! -x "$py" ]; then
		# A new mem0 environment is created where the path says: <env>/bin/python.
		[ "$provider" != mem0 ] || [[ $py == */bin/python ]] ||
			die "$py not found; give an existing interpreter, or a path ending in /bin/python to create an environment there"
		command -v uv >/dev/null || die "$py not found; install uv, then run: $install"
		[ "$(ask "$py not found. Run '$install'? (y/n)" y)" = y ] || die "install $provider, then run setup again"
		if [ "$provider" = mem0 ]; then
			uv venv "${py%/bin/python}"
			uv pip install --python "$py" "mem0ai==$version"
		else
			uv tool install "mnemosyne-memory==$version"
		fi
		[ -f "$py" ] && [ -x "$py" ] || die "$py still not found; give the interpreter path of the $provider install"
	fi
	settings=
	if [ "$provider" = mem0 ]; then
		settings=$(tilde "$(ask "JSON file with mem0's own config (llm, embedder, vector_store; see README, Choose a store)" "")")
		[ -f "$settings" ] || die "mem0 needs an llm and an embedder: write its config to a JSON file, then run setup again"
	fi

	echo "3. Store"
	new=
	case $(ask "Create a new store or use an existing one? (new/existing)" new) in
	new)
		data=$(tilde "$(ask "Directory for the new store" "$HOME/.local/share/memory-sharing-mcp/$provider")")
		new=1
		;;
	existing)
		data=$(tilde "$(ask "Directory holding $marker" "$existing_default")")
		[ -f "$data/$marker" ] || die "no $marker in $data"
		echo "   Turn off every agent's direct access to this store (such as an agent's own memory extension);"
		echo "   from now on only the service may open it."
		;;
	*) die "answer new or existing" ;;
	esac

	mkdir -p "$(dirname "$main")"
	# Written beside its final name and moved there only once it is known to
	# work: a config left behind by a failed setup would make every rerun skip
	# these steps.
	draft=$main.new
	trap 'rm -f "$draft"' EXIT
	if [ "$provider" = mem0 ]; then
		# The mem0 config may hold API keys.
		(umask 077 && printf '{\n  "provider": "mem0",\n  "mem0": {\n    "data_dir": %s,\n    "python": %s,\n    "config": %s\n  }\n}\n' \
			"$(json "$data")" "$(json "$py")" "$(cat "$settings")" >"$draft")
	else
		printf '{\n  "provider": "mnemosyne",\n  "mnemosyne": {\n    "data_dir": %s,\n    "python": %s\n  }\n}\n' \
			"$(json "$data")" "$(json "$py")" >"$draft"
	fi
	if [ -n "$new" ]; then
		"$bin" --config "$draft" store init
	else
		# Any command that reads the config checks that it parses.
		"$bin" --config "$draft" mcp keys list >/dev/null
	fi
	mv "$draft" "$main"
	echo "   config written to $main"
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

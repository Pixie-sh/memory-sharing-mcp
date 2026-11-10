#!/usr/bin/env bash
# Connect one agent to the memory-sharing-mcp service: give it its own API key
# and config file, then register it with the agent or print what to add.
#
#   AGENT=claude|opencode|pi|codex|<name> scripts/gen-config.sh
#
# Without AGENT it asks. Running it again for the same agent reuses its key.
set -euo pipefail

bin=$(command -v memory-sharing-mcp || echo "$HOME/.local/bin/memory-sharing-mcp")
main=${MSM_CONFIG:-$HOME/.config/memory-sharing-mcp/config.json}
dir=$(dirname "$main")
url=${URL:-http://127.0.0.1:8790}

die() { echo "error: $*" >&2; exit 1; }
json() { local s=${1//\\/\\\\}; printf '"%s"' "${s//\"/\\\"}"; }

echo "1. Checking the installation"
[ -x "$bin" ] || die "memory-sharing-mcp is not installed: run 'make install' (see Quick start in README.md)"
[ -f "$main" ] || die "no config at $main: write it as in README.md (Quick start, step 2), run 'memory-sharing-mcp store init', then 'make install'"
if curl -sf --max-time 2 "$url/readyz" >/dev/null; then
	echo "   service ready at $url"
else
	echo "   warning: no service at $url; the agent cannot use memory until it runs ('make install', 'make readiness')"
fi

agent=${AGENT:-}
if [ -z "$agent" ]; then
	[ -t 0 ] || die "set AGENT=claude|opencode|pi|codex|<name>"
	PS3="Agent to configure: "
	select agent in claude opencode pi codex other; do [ -n "$agent" ] && break; done
	if [ "$agent" = other ]; then read -rp "Agent name: " agent; fi
fi
[[ $agent =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "agent name '$agent': use lowercase letters, digits, - and _"

cfg=$dir/agents/$agent.json
echo "2. API key and config for $agent"
if [ -f "$cfg" ]; then
	echo "   reusing $cfg"
else
	key=$("$bin" --config "$main" mcp keygen --name "$agent")
	# ponytail: copies only "listen" from the main config, read with sed; enough
	# for the flat file the README describes. Upgrade path: a config command.
	listen=$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$main" | head -1)
	mkdir -p "$dir/agents"
	{
		printf '{\n  "credential_file": %s' "$(json "$key")"
		[ -z "$listen" ] || printf ',\n  "listen": %s' "$(json "$listen")"
		printf '\n}\n'
	} >"$cfg"
	chmod 600 "$cfg"
	echo "   key saved to $key"
	echo "   config written to $cfg"
fi

echo "3. Register the memory server with $agent"
case $agent in
claude)
	cmd=(claude mcp add --scope user memory -- "$bin" --config "$cfg" mcp serve)
	echo "   ${cmd[*]}"
	if command -v claude >/dev/null && [ -t 0 ]; then
		read -rp "   Run it now? [y/N] " yn
		if [ "$yn" = y ]; then
			"${cmd[@]}" || echo "   if 'memory' already exists, run 'claude mcp remove --scope user memory' first"
		fi
	fi
	;;
pi)
	echo "   Add to ~/.pi/agent/mcp-adapter.json (needs pi-mcp-adapter):"
	echo "   {\"mcpServers\": {\"memory\": {\"command\": $(json "$bin"), \"args\": [\"--config\", $(json "$cfg"), \"mcp\", \"serve\"], \"directTools\": true}}}"
	;;
opencode)
	echo "   Add to ~/.config/opencode/opencode.json:"
	echo "   {\"mcp\": {\"memory\": {\"type\": \"local\", \"command\": [$(json "$bin"), \"--config\", $(json "$cfg"), \"mcp\", \"serve\"], \"enabled\": true}}}"
	;;
codex)
	echo "   Add to ~/.codex/config.toml:"
	printf '   [mcp_servers.memory]\n   command = %s\n   args = ["--config", %s, "mcp", "serve"]\n' "$(json "$bin")" "$(json "$cfg")"
	;;
*)
	echo "   Add a stdio MCP server named 'memory' with this command:"
	echo "   $bin --config $cfg mcp serve"
	;;
esac
echo "4. Turn off $agent's own memory integration for the same store, restart $agent, and ask it to run memory_stats."

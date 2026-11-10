#!/usr/bin/env bash
# End-to-end smoke test against a COPY of a Mnemosyne store.
#
#   scripts/smoke.sh <mnemosyne python> <path to a copy of mnemosyne.db>
#
# The database is copied again into a temporary directory, so the file you pass
# is never written. Never pass a live store that another process is using.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <mnemosyne python> <mnemosyne.db copy>" >&2; exit 2; }
py=$1 db=$2
port=${MSM_SMOKE_PORT:-18790}
base=http://127.0.0.1:$port

work=$(mktemp -d)
chmod 700 "$work"
pid=
cleanup() {
	[ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

mkdir -m 700 "$work/data"
cp "$db" "$work/data/mnemosyne.db"
go build -o "$work/msm" ./cmd/memory-sharing-mcp
cat >"$work/config.json" <<EOF
{
  "listen": "127.0.0.1:$port",
  "endpoint": "$base/mcp",
  "mnemosyne": {"data_dir": "$work/data", "python": "$py"},
  "state_dir": "$work/state",
  "credentials_dir": "$work/creds"
}
EOF
msm() { "$work/msm" --config "$work/config.json" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

rw=$(msm mcp keygen --name smoke-rw --print 2>/dev/null)
ro=$(msm mcp keygen --name smoke-ro --role read-only --print 2>/dev/null)

# Not via the msm function: $! must be the server itself, not a subshell.
"$work/msm" --config "$work/config.json" http serve 2>"$work/serve.log" &
pid=$!
for _ in $(seq 100); do
	curl -fs "$base/readyz" >/dev/null && break
	sleep 0.2
done
curl -fs "$base/readyz" >/dev/null || { cat "$work/serve.log" >&2; fail "service not ready"; }
ok "http serve ready"

msm http serve 2>"$work/second.log" && fail "second owner started"
grep -q "already owned" "$work/second.log" || fail "second owner: $(cat "$work/second.log")"
ok "second owner refused"

# post <key> <op> <json> prints "<status> <body>".
post() {
	curl -s -o "$work/body" -w '%{http_code}' -H "Authorization: Bearer $1" \
		-H 'Content-Type: application/json' -d "$3" "$base/v1/$2"
	printf ' %s\n' "$(cat "$work/body")"
}
expect() { # expect <status> <description> <post args...>
	local want=$1 what=$2
	shift 2
	local got
	got=$(post "$@")
	[ "${got%% *}" = "$want" ] || fail "$what: got $got"
	ok "$what ($want)"
}

marker="smoke-$RANDOM-$RANDOM"
expect 200 "remember" "$rw" remember "{\"content\":\"memory-sharing-mcp $marker\"}"
expect 200 "recall" "$ro" recall "{\"query\":\"$marker\"}"
grep -q "$marker" "$work/body" || fail "recall did not return the stored memory"
grep -q '"id":"mnemosyne:' "$work/body" || fail "recall ids lack the provider prefix"
expect 200 "stats" "$ro" stats '{}'
expect 403 "read-only remember" "$ro" remember '{"content":"x"}'
expect 403 "read-write forget" "$rw" forget '{"id":"mnemosyne:x"}'
expect 401 "bad key" "msm_0000000000000000_AAAA" stats '{}'
expect 400 "invalid input" "$rw" recall '{"query":""}'

# stdio adapter: initialize, list tools, recall.
out=$({
	printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
	printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
	printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
	printf '%s\n' "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"memory_recall\",\"arguments\":{\"query\":\"$marker\"}}}"
	sleep 3
} | MSM_API_KEY=$ro msm mcp serve 2>/dev/null)
[ "$(grep -c '"name":"memory_' <<<"$out")" -ge 1 ] || fail "stdio: tools not listed"
grep -q "$marker" <<<"$out" || fail "stdio: recall did not return the stored memory"
ok "stdio adapter lists tools and recalls"

ro_id=$(msm mcp keys list | awk '$2 == "smoke-ro" {print $1}')
msm mcp keys revoke "$ro_id" >/dev/null 2>&1
sleep 2.5
expect 401 "revoked key" "$ro" stats '{}'

grep -q "$marker" "$work/serve.log" && fail "memory content in the service log"
grep -qF "$rw" "$work/serve.log" && fail "secret in the service log"
ok "no content or secrets in the log"

kill "$pid"
wait "$pid" 2>/dev/null || true
pid=
if command -v sqlite3 >/dev/null; then
	[ "$(sqlite3 "$work/data/mnemosyne.db" 'PRAGMA integrity_check')" = ok ] || fail "integrity check"
	ok "integrity check"
fi
echo "smoke test passed"

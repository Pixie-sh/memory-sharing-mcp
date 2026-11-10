"""Mnemosyne provider for memory-sharing-mcp.

Usage: python -I -c <script> <required mnemosyne-memory version> <session> open|create

Protocol: one JSON request per line on stdin, one JSON response per line on
stdout, strictly one request in flight. The first response (id 0) reports
readiness or a startup failure. Validation and policy live in the Go gateway;
this script maps each request to one Mnemosyne call.
"""
import json
import os
import sys
import traceback

# Keep the protocol stream private: library prints (Python or C level) go to stderr.
out = os.fdopen(os.dup(1), "w", buffering=1, encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr


def send(obj):
    out.write(json.dumps(obj, ensure_ascii=False, default=str) + "\n")


def fail(msg):
    send({"id": 0, "ok": False, "error": msg})
    sys.exit(1)


expected_version, SESSION, MODE = sys.argv[1], sys.argv[2], sys.argv[3]
data_dir = os.environ.get("MNEMOSYNE_DATA_DIR", "")
db_path = os.path.join(data_dir, "mnemosyne.db")
# Mnemosyne silently creates an empty store for a wrong path; refuse instead,
# unless asked to create one (store init).
if not data_dir or (MODE != "create" and not os.path.isfile(db_path)):
    fail(f"database not found: {db_path}")

import mnemosyne  # noqa: E402

if mnemosyne.__version__ != expected_version:
    fail(f"mnemosyne-memory {mnemosyne.__version__} found, {expected_version} required")

from mnemosyne.core.memory import Mnemosyne  # noqa: E402

reader = Mnemosyne(session_id=SESSION, db_path=db_path)
writers = {}


def writer(author_id):
    if author_id not in writers:
        writers[author_id] = Mnemosyne(
            session_id=SESSION, db_path=db_path, author_id=author_id, author_type="api-key"
        )
    return writers[author_id]


def remember(a):
    memory_id = writer(a["author_id"]).remember(
        a["content"],
        source=a["source"],
        importance=a["importance"],
        scope="session",
        extract_entities=True,  # as Pi's CLI path does
    )
    return {"id": memory_id}


def item(r):
    # The contract's fields (provider.Memory, mem0 names); scoring internals stay here.
    meta = {k: r[k] for k in ("source", "importance") if r.get(k) is not None}
    # recall cuts content to 500 characters; get() is a pure read of the whole memory,
    # needed only when the cut may have happened.
    full = reader.get(r["id"]) if len(r["content"]) >= 500 else None
    return {
        "id": r["id"],
        "memory": full["content"] if full else r["content"],
        "created_at": r.get("timestamp"),
        "agent_id": r.get("author_id"),
        "metadata": meta,
    }


def forget(a):
    if reader.forget(a["id"]):
        return {"deleted": True, "hidden": False}
    # 3.15.1 cannot delete episodic memories (consolidated by sleep). invalidate
    # expires the row, which every recall path skips; only this session's or
    # global-scope memories are reachable, so both false means not found here.
    return {"deleted": False, "hidden": reader.invalidate(a["id"])}


def recall(a):
    return {"results": [item(r) for r in reader.recall(a["query"], top_k=a["top_k"])]}


OPS = {
    "remember": remember,
    "recall": recall,
    "stats": lambda a: reader.get_stats(),
    "forget": forget,
    "sleep": lambda a: reader.sleep(),
}

send({"id": 0, "ok": True, "result": {"version": mnemosyne.__version__}})

for line in sys.stdin:
    req_id = 0
    try:
        req = json.loads(line)
        req_id = req["id"]
        result = OPS[req["op"]](req.get("args") or {})
    except Exception as e:  # reported to the caller as a tool error
        # The gateway forwards only the exception type to clients and logs
        # nothing; the traceback on stderr is the only diagnostic, and it is
        # seen only with provider_log enabled.
        traceback.print_exc()
        send({"id": req_id, "ok": False, "error": f"{type(e).__name__}: {e}"})
        continue
    send({"id": req_id, "ok": True, "result": result})

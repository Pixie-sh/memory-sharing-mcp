"""mem0 provider for memory-sharing-mcp.

Usage: python -I -c <script> <required mem0ai version> <user id> open|create

Environment: MEM0_DIR is the store directory; MSM_MEM0_CONFIG is mem0's own
configuration as JSON (llm, embedder, vector_store), kept out of argv because
it may hold API keys.

Protocol: see package pyproc. Validation and policy live in the Go gateway;
this script maps each request to one mem0 call.
"""
import json
import os
import sys
import traceback
import uuid
from importlib.metadata import PackageNotFoundError, version

# Keep the protocol stream private: library prints (Python or C level) go to stderr.
out = os.fdopen(os.dup(1), "w", buffering=1, encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr


def send(obj):
    out.write(json.dumps(obj, ensure_ascii=False, default=str) + "\n")


def fail(msg):
    send({"id": 0, "ok": False, "error": msg})
    sys.exit(1)


expected_version, USER, MODE = sys.argv[1], sys.argv[2], sys.argv[3]
data_dir = os.environ.get("MEM0_DIR", "")
history = os.path.join(data_dir, "history.db")
# mem0 silently creates an empty store for a wrong path; refuse instead,
# unless asked to create one (store init).
if not data_dir or (MODE != "create" and not os.path.isfile(history)):
    fail(f"store not found: {history}")

try:
    found_version = version("mem0ai")
except PackageNotFoundError:
    fail(f"mem0ai is not installed for {sys.executable}")
if found_version != expected_version:
    fail(f"mem0ai {found_version} found, {expected_version} required")

config = json.loads(os.environ.get("MSM_MEM0_CONFIG") or "{}")
if not isinstance(config, dict):
    fail("mem0.config must be an object")
if "history_db_path" in config:
    fail("mem0.config.history_db_path is set by the gateway (<data_dir>/history.db); remove it")
config["history_db_path"] = history
# Where the memories go is the operator's decision, never a default: mem0's own
# is a Qdrant store under /tmp.
store = config.get("vector_store")
if not isinstance(store, dict) or not store.get("provider") or not isinstance(store.get("config"), dict) or not store["config"]:
    fail("mem0.config.vector_store needs a provider and a config saying where memories are stored")
where = store["config"]
if where.get("path"):
    where["path"] = os.path.expanduser(where["path"])
    if not os.path.isabs(where["path"]):
        fail("mem0.config.vector_store.config.path must be absolute (or start with ~/)")
if store["provider"] == "qdrant":
    # mem0's own rule, checked here so that its /tmp default for path never applies.
    if not (where.get("path") or (where.get("host") and where.get("port")) or (where.get("url") and where.get("api_key"))):
        fail("mem0.config.vector_store.config needs path, host and port, or url and api_key")


def qdrant_collection_exists():
    name = where.get("collection_name") or "mem0"
    # The client parameters mem0 builds; with none of them it opens the local path.
    params = {k: where[k] for k in ("api_key", "url") if where.get(k)}
    if where.get("host") and where.get("port"):
        params.update(host=where["host"], port=where["port"])
    if where.get("https") is not None:
        params["https"] = where["https"]
    if not params:
        return os.path.isdir(os.path.join(where["path"], "collection", name))
    from qdrant_client import QdrantClient

    client = QdrantClient(**params)
    try:
        return client.collection_exists(name)
    finally:
        client.close()


# mem0 silently creates an empty collection for a wrong location, as it does
# for history.db; refuse that too. store init, in turn, refuses a collection
# that already exists: it may be another store's, in use.
# ponytail: checked for Qdrant only; other vector stores need their own
# existence check here before they can be trusted not to start empty.
if store["provider"] == "qdrant":
    try:
        found = qdrant_collection_exists()
    except Exception as e:
        traceback.print_exc()
        fail(f"mem0 vector store: {type(e).__name__}: {e}")
    if MODE != "create" and not found:
        fail("mem0.config.vector_store has no collection at the configured location; "
             "check it, or create a new store with store init")
    if MODE == "create" and found:
        fail("mem0.config.vector_store already has a collection at the configured location; "
             "use that store, or give a new location (or collection_name) for a new one")

from mem0 import Memory  # noqa: E402

try:
    m = Memory.from_config(config)
except Exception as e:
    traceback.print_exc()
    fail(f"mem0 configuration: {type(e).__name__}: {e}")

SCOPE = {"user_id": USER}


def remember(a):
    # ponytail: stored verbatim (infer=False), one memory per call, as the
    # contract's {id} needs. Upgrade path: infer=True lets mem0's LLM extract
    # facts, but returns zero or several memories and needs a contract change.
    added = m.add(
        a["content"],
        user_id=USER,
        agent_id=a["author_id"],
        metadata={"source": a["source"], "importance": a["importance"]},
        infer=False,
    )
    return {"id": added["results"][0]["id"]}


def item(r):
    # The contract's fields (provider.Memory); mem0's score and hash stay here.
    return {
        "id": r["id"],
        "memory": r["memory"],
        "created_at": r.get("created_at"),
        "updated_at": r.get("updated_at"),
        "agent_id": r.get("agent_id"),
        "metadata": r.get("metadata") or {},
    }


def recall(a):
    # threshold=0: mem0's default drops weak matches; the contract ranks them.
    found = m.search(a["query"], top_k=a["top_k"], filters=SCOPE, threshold=0.0)
    return {"results": [item(r) for r in found["results"]]}


def forget(a):
    # mem0 ids are UUIDs; anything else (such as another provider's id) is not found.
    try:
        uuid.UUID(a["id"])
    except ValueError:
        return {"deleted": False, "hidden": False}
    # Memories of another user_id in the same collection are not this store's.
    found = m.get(a["id"])
    if not found or found.get("user_id") != USER:
        return {"deleted": False, "hidden": False}
    m.delete(a["id"])
    return {"deleted": True, "hidden": False}


def stats(a):
    # points_count is Qdrant's; other vector stores report no count. It covers
    # the whole collection, whatever the user_id.
    info = m.vector_store.col_info()
    return {
        "version": found_version,
        "user_id": USER,
        "vector_store": store["provider"],
        "memories": getattr(info, "points_count", None),
    }


def sleep(a):
    raise NotImplementedError("mem0 has no consolidation step")


OPS = {"remember": remember, "recall": recall, "stats": stats, "forget": forget, "sleep": sleep}

send({"id": 0, "ok": True, "result": {"version": found_version}})

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

# stdin closed: the gateway is shutting the provider down. Close the local
# Qdrant client here; left to interpreter exit, its __del__ fails noisily.
close = getattr(m, "close", None)
if close:
    close()
close = getattr(getattr(m.vector_store, "client", None), "close", None)
if close:
    close()

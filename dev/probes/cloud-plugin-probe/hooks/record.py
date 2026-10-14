"""Record one hook event as a marker file under /tmp/aa-plugin-probe. Never uploads anything."""
import json, os, sys, time

d = "/tmp/aa-plugin-probe"
os.makedirs(d, exist_ok=True)
try:
    e = json.load(sys.stdin)
except ValueError:
    e = {}
def info(p):
    return {"path": p, "exists": bool(p) and os.path.exists(p),
            "bytes": os.path.getsize(p) if p and os.path.exists(p) else None}
rec = {
    "event": e.get("hook_event_name"), "source": e.get("source"), "session_id": e.get("session_id"),
    "transcript": info(e.get("transcript_path") or ""),
    "agent_transcript": info(e.get("agent_transcript_path") or ""),
    "input_keys": sorted(e.keys()),
    "CLAUDE_CODE_REMOTE": os.environ.get("CLAUDE_CODE_REMOTE"),
    "CLAUDE_PLUGIN_ROOT": os.environ.get("CLAUDE_PLUGIN_ROOT"),
    "time": time.time(),
}
with open(os.path.join(d, "%s-%d.json" % (rec["event"], time.time_ns())), "w") as f:
    json.dump(rec, f)

"""Record one hook event as a marker file under /tmp/aa-plugin-probe. Never uploads anything."""
import hashlib, json, os, sys, time

d = "/tmp/aa-plugin-probe"
os.makedirs(d, exist_ok=True)
try:
    e = json.load(sys.stdin)
except ValueError:
    e = {}
def info(p):
    return {"path": p, "exists": bool(p) and os.path.exists(p),
            "bytes": os.path.getsize(p) if p and os.path.exists(p) else None}
secret = os.environ.get("CLAUDE_PLUGIN_OPTION_UPLOAD_SECRET")
rec = {
    "event": e.get("hook_event_name"), "source": e.get("source"), "reason": e.get("reason"),
    "session_id": e.get("session_id"),
    "transcript": info(e.get("transcript_path") or ""),
    "agent_transcript": info(e.get("agent_transcript_path") or ""),
    "input_keys": sorted(e.keys()),
    "argv_note": sys.argv[1:],
    "option_env_keys": sorted(k for k in os.environ if k.startswith("CLAUDE_PLUGIN_OPTION_")),
    "upload_note": os.environ.get("CLAUDE_PLUGIN_OPTION_UPLOAD_NOTE"),
    "upload_secret": None if secret is None else {
        "length": len(secret), "sha256_prefix": hashlib.sha256(secret.encode()).hexdigest()[:12]},
    "CLAUDE_CODE_REMOTE": os.environ.get("CLAUDE_CODE_REMOTE"),
    "CLAUDE_PLUGIN_ROOT": os.environ.get("CLAUDE_PLUGIN_ROOT"),
    "CLAUDE_PLUGIN_DATA": os.environ.get("CLAUDE_PLUGIN_DATA"),
    "time": time.time(),
}
with open(os.path.join(d, "%s-%d.json" % (rec["event"], time.time_ns())), "w") as f:
    json.dump(rec, f)

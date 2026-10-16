"""SessionStart checks: bundled binary and GitHub release reachability. Writes /tmp only."""
import json, os, stat, subprocess, sys, time

d = "/tmp/aa-plugin-probe"
os.makedirs(d, exist_ok=True)
root = os.environ.get("CLAUDE_PLUGIN_ROOT", "")
b = os.path.join(root, "libexec", "agent-archive")
out = {"binary": {"path": b, "exists": os.path.exists(b)}}
if os.path.exists(b):
    st = os.stat(b)
    out["binary"].update({"bytes": st.st_size, "mode": oct(st.st_mode & 0o777),
                          "executable": bool(st.st_mode & stat.S_IXUSR)})
    try:
        p = subprocess.run([b, "version"], capture_output=True, text=True, timeout=15)
        out["binary"]["version_rc"] = p.returncode
        out["binary"]["version_out"] = (p.stdout + p.stderr)[:300]
    except Exception as ex:  # report, never fail the hook
        out["binary"]["version_error"] = repr(ex)[:300]
urls = ["https://github.com/wangjohn/agent-archive/releases",
        "https://api.github.com/repos/wangjohn/agent-archive/releases/latest",
        "https://objects.githubusercontent.com/"]
out["egress"] = {}
for u in urls:
    try:
        p = subprocess.run(["curl", "-sS", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}", u],
                           capture_output=True, text=True, timeout=15)
        out["egress"][u] = (p.stdout or p.stderr)[:120]
    except Exception as ex:
        out["egress"][u] = repr(ex)[:120]
out["time"] = time.time()
with open(os.path.join(d, "checks-%d.json" % time.time_ns()), "w") as f:
    json.dump(out, f)

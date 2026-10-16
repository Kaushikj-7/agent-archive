"""Archived pre-build research for blocked Claude Code cloud pull capture.

The command-line entry point is disabled. Do not run the retained functions
against a real account: they read login state and save unfiltered transcripts.

Answers, for the user's own account:
  1. Does /v1/code/sessions/{id}/teleport-events work with the Claude Code login,
     and do its records look like local Claude Code JSONL transcripts?
  2. How do the API session id (cse_/session_), the transcript sessionId and
     other ids relate?
  3. Are subagent (sidechain) records in the same stream?
  4. Does a `claude setup-token` token work (set AGENT_ARCHIVE_PROBE_TOKEN)?
  5. Can the login be read without a prompt from launchd (--auth-only)?
  6. How far back does the session list go, and which page sizes work?

Never prints or saves the token. Raw transcripts and a summary are written to
dev/probes/out/ (untracked; delete when done).

Usage:
  python3 -I dev/probes/claude_cloud_checks.py [--sessions N] [--id cse_...]
  AGENT_ARCHIVE_PROBE_TOKEN=... python3 -I dev/probes/claude_cloud_checks.py --token-check
  python3 -I dev/probes/claude_cloud_checks.py --auth-only   # for the launchd check
"""

import argparse
import collections
import glob
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://api.anthropic.com"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "out")
HOME = os.path.expanduser("~")


# ---------- login ----------

def read_login():
    """Returns (token, info) where info never contains secrets."""
    env = os.environ.get("AGENT_ARCHIVE_PROBE_TOKEN")
    if env:
        return env, {"source": "env AGENT_ARCHIVE_PROBE_TOKEN"}
    raw, source = None, None
    if sys.platform == "darwin":
        p = subprocess.run(
            ["security", "find-generic-password", "-s", "Claude Code-credentials", "-w"],
            capture_output=True, text=True,
        )
        if p.returncode == 0:
            raw, source = p.stdout, "macOS keychain 'Claude Code-credentials'"
        else:
            return None, {"source": "macOS keychain", "error": f"security exit {p.returncode}: {p.stderr.strip()[:200]}"}
    if raw is None:
        path = os.path.join(os.environ.get("CLAUDE_CONFIG_DIR", os.path.join(HOME, ".claude")), ".credentials.json")
        try:
            raw, source = open(path).read(), path
        except OSError as e:
            return None, {"source": path, "error": str(e)}
    doc = json.loads(raw)
    oauth = doc.get("claudeAiOauth", {})
    info = {
        "source": source,
        "credential_top_keys": sorted(doc.keys()),
        "oauth_keys": sorted(oauth.keys()),
        "scopes": oauth.get("scopes"),
        "subscription_type": oauth.get("subscriptionType"),
    }
    exp = oauth.get("expiresAt")
    if isinstance(exp, (int, float)):
        info["expires_in_minutes"] = round((exp / 1000 - time.time()) / 60, 1)
    rexp = oauth.get("refreshTokenExpiresAt")
    if isinstance(rexp, (int, float)):
        info["refresh_token_expires_in_days"] = round((rexp / 1000 - time.time()) / 86400, 1)
    return oauth.get("accessToken"), info


def read_org():
    try:
        d = json.load(open(os.path.join(HOME, ".claude.json")))
        return d.get("oauthAccount", {}).get("organizationUuid")
    except (OSError, ValueError):
        return None


# ---------- http ----------

class Api:
    def __init__(self, token, org):
        self.token, self.org = token, org

    def get(self, path, params=None, org=True, beta=None):
        url = BASE + path + ("?" + urllib.parse.urlencode(params) if params else "")
        h = {
            "Authorization": f"Bearer {self.token}",
            "anthropic-version": "2023-06-01",
            "Content-Type": "application/json",
            "User-Agent": "agent-archive-probe/0.1",
        }
        if org and self.org:
            h["x-organization-uuid"] = self.org
        if beta:
            h["anthropic-beta"] = beta
        req = urllib.request.Request(url, headers=h)
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, json.load(r)
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")[:300]
            return e.code, {"_error_body": body}
        except urllib.error.URLError as e:
            return 0, {"_error_body": str(e.reason)}


def keys_of(o):
    return sorted(o.keys()) if isinstance(o, dict) else type(o).__name__


# ---------- checks ----------

def check_list(api, report):
    sizes = {}
    for limit in (20, 100, 1000):
        st, body = api.get("/v1/code/sessions", {"limit": limit})
        sizes[limit] = {"status": st, "rows": len(body.get("data", [])) if st == 200 else None,
                        "error": body.get("_error_body") if st != 200 else None}
    report["list_page_sizes"] = sizes
    st0, _ = api.get("/v1/code/sessions", {"limit": 5}, org=False)
    report["list_without_org_header_status"] = st0

    limit = 100 if sizes[100]["status"] == 200 else 20
    rows, cursor, pages, top_keys, row_keys = [], None, 0, set(), set()
    while pages < 100:
        params = {"limit": limit}
        if cursor:
            params["cursor"] = cursor
        st, body = api.get("/v1/code/sessions", params)
        if st != 200:
            report["list_error"] = {"status": st, "body": body.get("_error_body")}
            break
        pages += 1
        top_keys |= set(body.keys())
        for r in body.get("data", []):
            row_keys |= set(r.keys())
            rows.append(r)
        cursor = body.get("next_cursor")
        if not cursor:
            break
    slim = []
    for r in rows:
        src = next((s for s in (r.get("config") or {}).get("sources") or [] if s.get("type") == "git_repository"), {})
        slim.append({
            "id": r.get("id"), "title": r.get("title"), "environment_kind": r.get("environment_kind"),
            "status": r.get("status"), "worker_status": r.get("worker_status"),
            "created_at": r.get("created_at"), "last_event_at": r.get("last_event_at"),
            "repo": src.get("url"), "tags": r.get("tags"),
        })
    kinds = collections.Counter(s["environment_kind"] for s in slim)
    created = sorted(s["created_at"] or "" for s in slim)
    report["list"] = {
        "page_size_used": limit, "pages": pages, "sessions": len(slim),
        "unique_ids": len({s["id"] for s in slim}), "by_environment_kind": dict(kinds),
        "oldest_created_at": created[0] if created else None,
        "newest_created_at": created[-1] if created else None,
        "response_top_keys": sorted(top_keys), "row_keys": sorted(row_keys),
        "id_prefixes": dict(collections.Counter((s["id"] or "").split("_")[0] for s in slim)),
        "ordered_by_last_event_desc": all(
            (slim[i]["last_event_at"] or "") >= (slim[i + 1]["last_event_at"] or "") for i in range(len(slim) - 1)),
    }
    json.dump(slim, open(os.path.join(OUT, "sessions.json"), "w"), indent=1)
    return slim


def summarize_records(recs):
    types = collections.Counter(str(r.get("type")) for r in recs if isinstance(r, dict))
    keys = collections.Counter(k for r in recs if isinstance(r, dict) for k in r)
    return {
        "records": len(recs),
        "types": dict(types),
        "sidechain_records": sum(1 for r in recs if isinstance(r, dict) and r.get("isSidechain")),
        "distinct_sessionId": sorted({str(r.get("sessionId")) for r in recs if isinstance(r, dict) and "sessionId" in r})[:5],
        "distinct_agentId": sorted({str(r.get("agentId")) for r in recs if isinstance(r, dict) and r.get("agentId")})[:10],
        "distinct_cwd": sorted({str(r.get("cwd")) for r in recs if isinstance(r, dict) and r.get("cwd")})[:5],
        "distinct_entrypoint": sorted({str(r.get("entrypoint")) for r in recs if isinstance(r, dict) and r.get("entrypoint")}),
        "versions": sorted({str(r.get("version")) for r in recs if isinstance(r, dict) and r.get("version")})[:5],
        "top_level_keys": dict(keys.most_common()),
        "with_uuid": sum(1 for r in recs if isinstance(r, dict) and r.get("uuid")),
        "first_timestamp": next((r.get("timestamp") for r in recs if isinstance(r, dict) and r.get("timestamp")), None),
        "last_timestamp": next((r.get("timestamp") for r in reversed(recs) if isinstance(r, dict) and r.get("timestamp")), None),
    }


def unwrap(item):
    """teleport-events items may wrap the transcript record; report and unwrap."""
    if isinstance(item, dict):
        for k in ("payload", "data", "event", "entry", "logline"):
            if isinstance(item.get(k), dict) and ("uuid" in item[k] or "type" in item[k]):
                return item[k], k
    return item, None


def fetch_teleport(api, sid):
    recs, wrappers, pages, cursor, top_keys, item_keys = [], collections.Counter(), 0, None, set(), set()
    while pages < 100:
        params = {"limit": 1000}
        if cursor is not None:
            params["cursor"] = cursor
        st, body = api.get(f"/v1/code/sessions/{sid}/teleport-events", params)
        if st != 200:
            return {"status": st, "error": body.get("_error_body"), "pages": pages}, recs
        pages += 1
        top_keys |= set(body.keys())
        data = body.get("data") or []
        for it in data:
            if isinstance(it, dict):
                item_keys |= set(it.keys())
            rec, w = unwrap(it)
            wrappers[w or "(none)"] += 1
            recs.append(rec)
        cursor = body.get("next_cursor")
        if not cursor or not data:
            break
    return {"status": 200, "pages": pages, "response_top_keys": sorted(top_keys),
            "item_keys": sorted(item_keys), "wrapper_field": dict(wrappers)}, recs


def check_session(api, sid):
    out = {"id": sid}
    st, meta = api.get(f"/v1/code/sessions/{sid}")
    out["get_session"] = {"status": st, "keys": keys_of(meta)}
    if st == 200:
        json.dump(meta, open(os.path.join(OUT, f"{sid}.meta.json"), "w"), indent=1)

    tinfo, trecs = fetch_teleport(api, sid)
    out["teleport_events"] = tinfo
    if trecs:
        with open(os.path.join(OUT, f"{sid}.teleport.jsonl"), "w") as f:
            for r in trecs:
                f.write(json.dumps(r) + "\n")
        out["teleport_events"]["summary"] = summarize_records(trecs)

    suffix = sid.split("_", 1)[1] if "_" in sid else sid
    st, body = api.get(f"/v1/session_ingress/session/session_{suffix}")
    lines = body.get("loglines") if isinstance(body, dict) else None
    out["session_ingress"] = {"status": st, "response_keys": keys_of(body),
                              "error": body.get("_error_body") if st != 200 else None}
    if isinstance(lines, list):
        with open(os.path.join(OUT, f"{sid}.ingress.jsonl"), "w") as f:
            for r in lines:
                f.write(json.dumps(r) + "\n")
        out["session_ingress"]["summary"] = summarize_records(lines)
        tu = {r.get("uuid") for r in trecs if isinstance(r, dict) and r.get("uuid")}
        iu = {r.get("uuid") for r in lines if isinstance(r, dict) and r.get("uuid")}
        out["teleport_vs_ingress_uuids"] = {"teleport": len(tu), "ingress": len(iu), "both": len(tu & iu),
                                            "only_teleport": len(tu - iu), "only_ingress": len(iu - tu)}

    st, body = api.get(f"/v1/code/sessions/{sid}/events", {"limit": 200, "sort_order": "asc"},
                       beta="ccr-triggers-2026-01-30")
    evs = body.get("data") if isinstance(body, dict) else None
    out["events_endpoint"] = {"status": st, "response_keys": keys_of(body),
                              "event_types": dict(collections.Counter(str(e.get("type")) for e in evs or [] if isinstance(e, dict))),
                              "item_keys": sorted({k for e in evs or [] if isinstance(e, dict) for k in e})}

    alt = ("session_" + suffix) if sid.startswith("cse_") else ("cse_" + suffix)
    st, _ = api.get(f"/v1/code/sessions/{alt}")
    out["id_alias"] = {"alias": alt, "get_status": st}
    return out


def local_baseline():
    """Type/key profile of recent local Claude Code transcripts, for comparison."""
    files = sorted(glob.glob(os.path.join(HOME, ".claude", "projects", "*", "*.jsonl")),
                   key=lambda p: os.path.getmtime(p), reverse=True)[:5]
    recs = []
    for p in files:
        with open(p) as f:
            for line in f:
                try:
                    recs.append(json.loads(line))
                except ValueError:
                    pass
    s = summarize_records(recs)
    s["files"] = len(files)
    for k in ("distinct_sessionId", "distinct_agentId", "distinct_cwd"):
        s.pop(k, None)
    return s


def dump_events(api, sid):
    """Page the full raw /events log of one session and profile its payloads."""
    items, cursor, pages, statuses = [], None, 0, []
    while pages < 500:
        params = {"limit": 200, "sort_order": "asc"}
        if cursor:
            params["cursor"] = cursor
        st, body = api.get(f"/v1/code/sessions/{sid}/events", params, beta="ccr-triggers-2026-01-30")
        statuses.append(st)
        if st != 200:
            break
        pages += 1
        data = body.get("data") or []
        items += data
        cursor = body.get("next_cursor")
        if not cursor or not data:
            break
    with open(os.path.join(OUT, f"{sid}.events.jsonl"), "w") as f:
        for it in items:
            f.write(json.dumps(it) + "\n")
    payloads = [it.get("payload") for it in items if isinstance(it, dict)]
    rep = {
        "id": sid, "pages": pages, "last_status": statuses[-1] if statuses else None, "events": len(items),
        "event_type": dict(collections.Counter(str(it.get("event_type")) for it in items)),
        "source": dict(collections.Counter(str(it.get("source")) for it in items)),
        "first_created_at": items[0].get("created_at") if items else None,
        "last_created_at": items[-1].get("created_at") if items else None,
        "payload_type_subtype": dict(collections.Counter(
            f"{p.get('type')}/{p.get('subtype')}" for p in payloads if isinstance(p, dict)).most_common(40)),
        "payload_keys": dict(collections.Counter(k for p in payloads if isinstance(p, dict) for k in p).most_common(60)),
        "payloads_with_uuid": sum(1 for p in payloads if isinstance(p, dict) and p.get("uuid")),
        "payloads_with_parent_tool_use_id": sum(1 for p in payloads if isinstance(p, dict) and p.get("parent_tool_use_id")),
        "sequence_monotonic": all((items[i].get("sequence_num") or 0) < (items[i + 1].get("sequence_num") or 0)
                                  for i in range(len(items) - 1)),
    }
    tele = os.path.join(OUT, f"{sid}.teleport.jsonl")
    if os.path.exists(tele):
        tu = {json.loads(l).get("uuid") for l in open(tele)}
        eu = {p.get("uuid") for p in payloads if isinstance(p, dict) and p.get("uuid")}
        rep["uuid_overlap_with_teleport"] = {"teleport": len(tu), "events": len(eu), "both": len(tu & eu)}
    json.dump(rep, open(os.path.join(OUT, f"{sid}.events-report.json"), "w"), indent=1)
    print(json.dumps({k: rep[k] for k in ("events", "pages", "last_status", "first_created_at", "last_created_at",
                                          "payloads_with_uuid", "payloads_with_parent_tool_use_id")}))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--events", help="dump the full raw /events log of this session id")
    ap.add_argument("--sessions", type=int, default=3, help="most recent anthropic_cloud sessions to fetch")
    ap.add_argument("--id", action="append", default=[], help="extra session id to fetch")
    ap.add_argument("--token-check", action="store_true", help="only list + one transcript (for setup-token)")
    ap.add_argument("--auth-only", action="store_true", help="only test reading the login (launchd check)")
    a = ap.parse_args()
    os.makedirs(OUT, exist_ok=True)

    token, info = read_login()
    if a.auth_only:
        result = {"time": time.strftime("%Y-%m-%dT%H:%M:%S"), "ok": bool(token), **info,
                  "ppid_cmd": subprocess.run(["ps", "-o", "comm=", "-p", str(os.getppid())],
                                             capture_output=True, text=True).stdout.strip()}
        if token:
            st, _ = Api(token, read_org()).get("/v1/code/sessions", {"limit": 1})
            result["list_status"] = st
        json.dump(result, open(os.path.join(OUT, "auth-check.json"), "w"), indent=1)
        print(json.dumps(result, indent=1))
        return
    if not token:
        sys.exit(f"no login: {info}")

    org = read_org()
    api = Api(token, org)
    if a.events:
        dump_events(api, a.events)
        return
    report = {"login": info, "org_uuid_found": bool(org)}
    slim = check_list(api, report)
    cloud = [s["id"] for s in slim if s["environment_kind"] == "anthropic_cloud"]
    targets = a.id + [i for i in cloud if i not in a.id][: (1 if a.token_check else a.sessions)]
    report["sessions_checked"] = [check_session(api, sid) for sid in targets]
    if not a.token_check:
        report["local_baseline"] = local_baseline()

    name = "token-check-report.json" if a.token_check else "report.json"
    json.dump(report, open(os.path.join(OUT, name), "w"), indent=1)
    l = report.get("list", {})
    print(f"login: {info.get('source')}  org header: {'yes' if org else 'MISSING'}")
    print(f"list: {l.get('sessions')} sessions over {l.get('pages')} pages, kinds={l.get('by_environment_kind')}, "
          f"oldest={l.get('oldest_created_at')}, page sizes={ {k: v['status'] for k, v in report['list_page_sizes'].items()} }")
    for s in report["sessions_checked"]:
        t = s["teleport_events"]
        ts = t.get("summary", {})
        print(f"{s['id']}: meta {s['get_session']['status']}, teleport-events {t['status']} "
              f"({ts.get('records', 0)} records, {ts.get('sidechain_records', 0)} sidechain, types={ts.get('types')}), "
              f"ingress {s['session_ingress']['status']}, events {s['events_endpoint']['status']}, alias {s['id_alias']['get_status']}")
    print(f"full report: {os.path.join(OUT, name)}")


if __name__ == "__main__":
    sys.exit("Blocked research probe: Claude Code API pull is not supported; "
             "no credentials were read and no requests were sent.")

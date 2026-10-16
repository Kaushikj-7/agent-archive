"""Async Stop hook: does a 20-second background task get to finish? Writes /tmp only."""
import json, os, sys, time

d = "/tmp/aa-plugin-probe"
os.makedirs(d, exist_ok=True)
t0 = time.time_ns()
open(os.path.join(d, "StopAsync-start-%d.json" % t0), "w").write(json.dumps({"t": t0}))
time.sleep(20)
open(os.path.join(d, "StopAsync-done-%d.json" % t0), "w").write(json.dumps({"t": t0, "slept": 20}))

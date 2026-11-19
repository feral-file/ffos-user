#!/usr/bin/env python3
"""Reject incomplete or mixed output evidence before publishing comparisons."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
class SummaryTest(unittest.TestCase):
    def check_run(self, mutation=None, expected=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run = dict(result="measured", unit="ff-native-runtime-test.service",
                       version={"Browser":"Chrome/1", "V8-Version":"v", "WebKit-Version":"w"},
                       fixture={}, document={}, clockStart={"monotonicNs":1_000_000_000},
                       clockEnd={"monotonicNs":4_000_000_000}, frameIntervals={}, callbackDelivery={},
                       cpuPercentOneCore=1, after={"MemoryCurrent":1,"MemoryPeak":1})
            times = list(range(900_000_000,4_100_000_000,100_000_000))
            timing = dict(waylandPresentation=dict(available=True,clockId=1,truncatedTimestamps=0,
                                                  changedTimestampsNs=times),
                          gpuCopies=1,cpuCopies=0,coalescedFrames=0)
            if mutation:
                mutation(run,timing)
            path=root/"run.json"
            path.write_text(json.dumps(run))
            path.with_suffix(".timing.json").write_text(json.dumps(timing))
            result=subprocess.run([sys.executable,str(ROOT/"tools/summarize-matched.py"),
                                   str(root/"out.json"),str(path)],capture_output=True,text=True)
            if expected:
                self.assertNotEqual(result.returncode,0)
                self.assertIn(expected,result.stderr)
            else:
                self.assertEqual(result.returncode,0,result.stderr)
    def test_complete(self):
        self.check_run()
    def test_public_kiosk_uses_wayland_feedback(self):
        def change(run, _timing):
            run["unit"] = "chromium-kiosk.service"
            run["configuration"] = {"presentationPath": "public"}
        self.check_run(change)
    def test_named_public_probe_uses_wayland_feedback(self):
        self.check_run(lambda run, _: run.update(unit="ff-player-public-test.service"))
    def test_short_prefix(self):
        self.check_run(lambda r,t:t["waylandPresentation"].update(changedTimestampsNs=list(range(900_000_000,2_500_000_000,100_000_000))),"does not cover")
    def test_missing_start(self):
        self.check_run(lambda r,t:t["waylandPresentation"].update(changedTimestampsNs=list(range(1_100_000_000,4_100_000_000,100_000_000))),"does not cover sample start")
    def test_duplicate(self):
        self.check_run(lambda r,t:t["waylandPresentation"]["changedTimestampsNs"].insert(5,1_300_000_000),"Non-monotonic")
    def test_multiple_streams(self):
        def change(r,t):
            r["unit"]="ff-native-chrome-test.service"
            r["compositorTrace"]={"events":[dict(ts=v/1000,pid=i%2,tid=1) for i,v in enumerate(t["waylandPresentation"]["changedTimestampsNs"])]}
        self.check_run(change,"Multiple output streams")

if __name__ == "__main__":
    unittest.main()

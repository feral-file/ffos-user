"""Safety regressions: malformed packs must fail before starting an engine."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

RUNNER=Path(__file__).with_name('browser_trial.py')
class IndexChecks(unittest.TestCase):
    def refuse(self, record):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);pack=root/'pack';pack.mkdir();(pack/'bodies').mkdir()
            (pack/'responses.json').write_text(json.dumps({'GET https://example.org/':record}))
            result=subprocess.run(['python3',str(RUNNER),'--engine','/engine-must-not-start',
                '--url','https://example.org/','--output',str(root/'output'),'--replay',str(pack)],capture_output=True,text=True)
            self.assertNotEqual(result.returncode,0)
            self.assertNotIn('/engine-must-not-start',result.stderr)
            return result.stderr
    def test_digest_path_traversal(self):
        self.assertIn('invalid body digest',self.refuse(dict(sha256='../secret',size=1,status=200)))
    def test_oversized_resource(self):
        self.assertIn('replay resource exceeds bound',self.refuse(dict(sha256='a'*64,size=33*1024*1024,status=200)))
    def test_missing_blob(self):
        self.assertIn('FileNotFoundError',self.refuse(dict(sha256='a'*64,size=1,status=200)))
if __name__=='__main__':unittest.main()

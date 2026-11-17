#!/usr/bin/env python3
"""Exercise atomic migration and refusal cases using the real C++ validator."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest

validator = Path(sys.argv.pop(1)).resolve()
root = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('migration', root/'tools/migrate-legacy.py')
migration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(migration)

def fixture():
    item = {'id':'art','source':'https://example.org/art','contentRating':'general'}
    playlist = {'id':'original','items':[item,copy.deepcopy(item)],'signature':'unaltered'}
    cast = {'playlist':playlist,'index':1,'contentContext':'personal','loopMode':'one','shuffle':True,'isPaused':True,'renderStatus':{'state':'ready'}}
    records = dict.fromkeys(migration.KEYS)
    for key, value in {
        'castInfo':cast, 'displaySettings':{'isSaved':True,'framing':'fill','tombstone':'off','scaling':'fit'},
        'defaultItemDuration':12.5, 'contentPolicy':{'version':1,'showMatureContent':False,'strictPersonal':True,'blockUnratedCurated':False},
        'boot_playlist':{'playlist':playlist,'contentContext':'personal'},
        'dp1_scheduled_tasks':{'id':'later','scheduleTime':'2030-01-02T03:04:05+07:00','dp1CallData':playlist,'contentContext':'curated'},
        'recentlyPlayed':[{'recordId':'rp-5','playedAtMs':12345,'item':item,'contentContext':'personal','defaults':{'display':{'scaling':'fill'}}}],
        'recentlyPlayedIncomplete':True,
    }.items(): records[key] = json.dumps(value)
    records['viewMode'] = 'portrait'
    return {'format':'ff-player-storage-export','version':2,'origin':'http://127.0.0.1:8080','exportedAt':'2026-11-17T00:00:00Z',
            'records':records,'runtime':{'route':'/sleep','bootHydration':'ok','hasArtwork':False,'cast':cast}}

class MigrationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='ff-migration-test-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.source = self.directory/'export.json'
        self.target = self.directory/'native'

    def run_import(self, document):
        self.source.write_bytes(migration.encode(document))
        return migration.migrate(self.source,self.target,validator)

    def state(self):
        with sqlite3.connect(self.target/'state.sqlite') as database:
            return json.loads(database.execute('SELECT document FROM state WHERE id=1').fetchone()[0])

    def test_intent_and_preferences_survive(self):
        original = fixture()
        provenance = self.run_import(original)
        state = self.state()
        self.assertTrue(state['sleeping']); self.assertFalse(state['stopped'])
        self.assertTrue(state['paused']); self.assertEqual(state['index'],1)
        self.assertEqual(state['playlist']['signature'],'unaltered')
        self.assertEqual(state['settings'],{'framing':'fill','tombstone':'off','scaling':'fit','durationSeconds':12.5})
        self.assertEqual(state['boot']['context'],'personal')
        self.assertEqual(state['schedule']['atMs'],1893528245000)
        self.assertEqual(state['history'][0]['context'],'personal')
        self.assertTrue(state['historyIncomplete'])
        self.assertNotIn('renderStatus',state); self.assertNotIn('presented',state)
        self.assertNotIn('rotation',state['settings'])
        self.assertEqual(provenance['legacyViewMode'],'portrait')
        self.assertEqual((self.target/'legacy-export.json').read_bytes(),self.source.read_bytes())
        self.assertEqual(self.target.stat().st_mode & 0o777,0o700)

    def test_stopped_empty_and_error_remain_stopped(self):
        for route, cast in [('/',None),('/error',json.loads(fixture()['records']['castInfo']))]:
            with self.subTest(route=route):
                document = fixture(); document['runtime'].update(route=route,hasArtwork=False)
                document['records']['castInfo'] = json.dumps(cast)
                normalized = migration.normalize(document,'0'*64)['state']
                self.assertTrue(normalized['stopped']); self.assertFalse(normalized['sleeping'])

    def test_existing_target_never_overwritten(self):
        self.run_import(fixture())
        before = (self.target/'state.sqlite').read_bytes()
        with self.assertRaises(ValueError): self.run_import(fixture())
        self.assertEqual((self.target/'state.sqlite').read_bytes(),before)

    def test_invalid_persisted_fields_never_published(self):
        for key, value in [('contentPolicy',{}),('recentlyPlayedIncomplete',0),('displaySettings',{'marginLeft':20}),
                           ('recentlyPlayed',[{'recordId':'broken'}]),('defaultItemDuration',0)]:
            with self.subTest(key=key):
                document = fixture(); document['records'][key] = json.dumps(value)
                with self.assertRaises((ValueError,subprocess.CalledProcessError)): self.run_import(document)
                self.assertFalse(self.target.exists())
                self.assertFalse(list(self.directory.glob('.ff-migration-*')))

    def test_unresolved_or_moving_legacy_state_is_refused(self):
        for change in ('v1','hydrating','thermal','daily','moved','unknown-viewport'):
            with self.subTest(change=change):
                document = fixture()
                if change=='v1': document['version']=1
                elif change=='hydrating': document['runtime']['bootHydration']='pending'
                elif change=='thermal': document['records']['criticalTemp']='true'
                elif change=='daily':
                    cast=json.loads(document['records']['castInfo']);cast['castCommand']='castDaily';document['records']['castInfo']=json.dumps(cast)
                elif change=='moved': document['runtime'].update(hasArtwork=True,cast={'playlist':None})
                else: document['records']['viewMode']='upside-down'
                with self.assertRaises(ValueError): self.run_import(document)
                self.assertFalse(self.target.exists())

    def test_duplicate_json_keys_are_not_lost(self):
        self.source.write_text('{"version":2,"version":1}')
        with self.assertRaises(ValueError): migration.migrate(self.source,self.target,validator)

    def test_malformed_dp1_identifiers_are_not_published(self):
        for field in ('playlist','item'):
            with self.subTest(field=field):
                document=fixture(); cast=json.loads(document['records']['castInfo'])
                if field=='playlist': cast['playlist']['id']=123
                else: cast['playlist']['items'][0]['id']=123
                document['records']['castInfo']=json.dumps(cast)
                with self.assertRaises(subprocess.CalledProcessError): self.run_import(document)
                self.assertFalse(self.target.exists())

    def test_publication_race_keeps_existing_directory(self):
        staging=self.directory/'stage';staging.mkdir();(staging/'new').write_text('new')
        self.target.mkdir();(self.target/'old').write_text('old')
        with self.assertRaises(FileExistsError): migration.publish_no_replace(staging,self.target)
        self.assertEqual((self.target/'old').read_text(),'old')
        self.assertTrue(staging.exists())

if __name__=='__main__': unittest.main()

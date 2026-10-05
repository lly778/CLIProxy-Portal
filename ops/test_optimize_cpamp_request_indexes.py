import importlib.util
import sqlite3
import tempfile
import unittest
from contextlib import closing
from pathlib import Path

spec = importlib.util.spec_from_file_location('indexes', Path(__file__).with_name('optimize-cpamp-request-indexes.py'))
indexes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(indexes)


class IndexTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'usage.sqlite'
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute('CREATE TABLE usage_events(id INTEGER PRIMARY KEY, api_key_hash TEXT, timestamp_ms INTEGER)')
            db.execute('CREATE TABLE usage_monitoring_event_projection_v1(event_id INTEGER PRIMARY KEY, api_key_hash TEXT, timestamp_ms INTEGER)')
            db.execute("INSERT INTO usage_events VALUES(1, 'a', 100)")

    def test_preview_apply_idempotence_and_query_plan(self):
        self.assertEqual(len(indexes.optimize(self.path)), 2)
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("SELECT count(*) FROM sqlite_master WHERE type='index'").fetchone()[0], 0)
        self.assertEqual(len(indexes.optimize(self.path, True)), 2)
        self.assertEqual(indexes.optimize(self.path, True), [])
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute('SELECT * FROM usage_events').fetchall(), [(1, 'a', 100)])
            for name, (table, identity, _) in indexes.INDEXES.items():
                plan = db.execute("EXPLAIN QUERY PLAN SELECT " + identity + " FROM " + table + " WHERE coalesce(api_key_hash, '') IN (SELECT value FROM json_each(?)) AND timestamp_ms >= ? AND timestamp_ms < ? ORDER BY timestamp_ms DESC, " + identity + " DESC LIMIT 100", ('["a"]', 0, 200)).fetchall()
                self.assertIn(name, str(plan))

    def test_schema_failure_makes_no_partial_changes(self):
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute('DROP TABLE usage_monitoring_event_projection_v1')
        with self.assertRaises(ValueError):
            indexes.optimize(self.path, True)
        with closing(sqlite3.connect(self.path)) as db, db:
            self.assertEqual(db.execute("SELECT count(*) FROM sqlite_master WHERE type='index'").fetchone()[0], 0)

    def test_conflicting_index_is_not_overwritten(self):
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute('CREATE INDEX idx_portal_usage_events_key_time ON usage_events(timestamp_ms)')
        with self.assertRaises(ValueError):
            indexes.optimize(self.path, True)


if __name__ == '__main__':
    unittest.main()

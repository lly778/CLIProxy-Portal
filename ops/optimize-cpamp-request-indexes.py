#!/usr/bin/env python3
"""Add reversible API-key lookup indexes for CPAMP 1.14.1 event queries.

No event rows, credentials, or configuration are modified. Run without --apply
to validate the schema and preview missing indexes. Re-run after a rebuild from
a backup predating these indexes. CPAMP's coalesce expression must match exactly.
"""
import argparse
import sqlite3
from contextlib import closing
from pathlib import Path

INDEXES = {
    'idx_portal_usage_events_key_time': (
        'usage_events', 'id',
        "CREATE INDEX idx_portal_usage_events_key_time ON usage_events(coalesce(api_key_hash, ''), timestamp_ms DESC, id DESC)",
    ),
    'idx_portal_monitoring_events_key_time': (
        'usage_monitoring_event_projection_v1', 'event_id',
        "CREATE INDEX idx_portal_monitoring_events_key_time ON usage_monitoring_event_projection_v1(coalesce(api_key_hash, ''), timestamp_ms DESC, event_id DESC)",
    ),
}


def optimize(path, apply=False):
    path = Path(path).resolve(strict=True)
    if not path.is_file():
        raise ValueError('database must be an existing file')
    mode = 'rw' if apply else 'ro'
    with closing(sqlite3.connect(path.as_uri() + '?mode=' + mode, uri=True, timeout=5)) as db, db:
        if apply:
            db.execute('BEGIN IMMEDIATE')
        else:
            db.execute('PRAGMA query_only=ON')
        missing = []
        for name, (table, identity, sql) in INDEXES.items():
            columns = {row[1] for row in db.execute('PRAGMA table_info(' + table + ')')}
            if not {'api_key_hash', 'timestamp_ms', identity}.issubset(columns):
                raise ValueError('unsupported CPAMP schema: ' + table)
            existing = db.execute('SELECT sql FROM sqlite_master WHERE name=?', (name,)).fetchone()
            if existing:
                if existing[0] != sql:
                    raise ValueError('index name already used by a different definition: ' + name)
            else:
                missing.append((name, sql))
        for name, sql in missing:
            if apply:
                db.execute(sql)
            print(('created: ' if apply else 'would create: ') + name)
        return [name for name, _ in missing]


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('database', help='host path to CPAMP usage.sqlite')
    parser.add_argument('--apply', action='store_true', help='create indexes (default: read-only preview)')
    args = parser.parse_args()
    optimize(args.database, args.apply)

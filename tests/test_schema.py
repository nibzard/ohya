"""Run with python3 -m unittest discover -s tests."""
import pathlib
import sqlite3
import unittest


class AnnotationSchemaTest(unittest.TestCase):
    def setUp(self):
        self.db = sqlite3.connect(':memory:')
        self.db.executescript(pathlib.Path('ohya_schema.sql').read_text())
        self.db.execute("INSERT INTO sessions(id,agent,device_id,native_id) VALUES('s','agent','d','n')")
        self.db.execute("INSERT INTO sources(id,device_id,agent,path) VALUES('f','d','agent','log')")
        self.db.execute("INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(1,'s','f',0,'message','{}','hello')")
        self.db.commit()

    def tearDown(self):
        self.db.close()

    def add(self, event=1, value='true', writer='d'):
        self.db.execute('INSERT INTO event_annotations VALUES(?,?,?,?,?,?,?)',
                        (event, 'security', 'review', value, writer, 1, 1))

    def test_writer_isolation_and_source_preservation(self):
        self.add()
        self.add(writer='other')
        self.db.execute("UPDATE event_annotations SET value='false',updated_at_ms=2 WHERE writer_id='d'")
        self.assertEqual(self.db.execute("SELECT value FROM event_annotations WHERE writer_id='other'").fetchone(), ('true',))
        self.assertEqual(self.db.execute('SELECT raw_json FROM events').fetchone(), ('{}',))
        self.assertEqual(self.db.execute("SELECT count(*) FROM events_fts WHERE events_fts MATCH 'hello'").fetchone(), (1,))

    def test_constraints_and_atomic_rollback(self):
        for event, value in [(99, 'true'), (1, 'invalid')]:
            with self.assertRaises(sqlite3.IntegrityError):
                with self.db:
                    self.add(writer='first')
                    self.add(event=event, value=value)
            self.assertEqual(self.db.execute('SELECT count(*) FROM event_annotations').fetchone(), (0,))
        self.add()
        with self.assertRaises(sqlite3.IntegrityError):
            self.add()


if __name__ == '__main__':
    unittest.main()

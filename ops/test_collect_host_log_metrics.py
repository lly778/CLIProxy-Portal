import importlib.util
import pathlib
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("collector", pathlib.Path(__file__).with_name("collect-host-log-metrics.py"))
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class StorageTests(unittest.TestCase):
    def test_data_size_includes_native_index_wal_and_logs_without_following_links(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            for name, size in [("agy/conversations/session.db", 100), ("portal-sessions.db", 20), ("portal-sessions.db-wal", 10), ("agy/log/cli.log", 30)]:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"x" * size)
            self.assertEqual(collector.directory_size(root), 160)
            # Linux production quota-home contains this back-reference.
            try:
                (root / "linked-agy").symlink_to(root / "agy", target_is_directory=True)
            except OSError:
                pass
            self.assertEqual(collector.directory_size(root), 160)
            self.assertEqual(collector.directory_size(root / "absent"), 0)
            self.assertIsNone(collector.directory_size(root / "portal-sessions.db"))
            with mock.patch.object(collector.os, "scandir", side_effect=PermissionError):
                self.assertIsNone(collector.directory_size(root))

    def test_collect_adds_just_metadata_bytes(self):
        with tempfile.TemporaryDirectory() as temp, mock.patch.object(collector, "docker_log_size", return_value=4):
            root = pathlib.Path(temp)
            (root / "main.log").write_bytes(b"log")
            data = collector.collect(root, root / "missing-agy")
            self.assertEqual(data["agy_data_bytes"], 0)
            self.assertEqual(data["cpa_main_log_bytes"], 3)
            self.assertEqual(data["container_log_bytes"], {"portal": 4, "cpamp": 4, "cpa": 4})


if __name__ == "__main__":
    unittest.main()

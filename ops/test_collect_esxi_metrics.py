import importlib.util
import pathlib
import tempfile
import unittest
from unittest import mock
import hashlib
import xml.etree.ElementTree as ET


spec = importlib.util.spec_from_file_location("esxi", pathlib.Path(__file__).with_name("collect-esxi-metrics.py"))
esxi = importlib.util.module_from_spec(spec)
spec.loader.exec_module(esxi)


class ESXiMetricsTests(unittest.TestCase):
    def values(self):
        return {"runtime.connectionState": "connected", "summary.hardware.numCpuCores": "4",
                "summary.hardware.cpuMhz": "2000", "summary.quickStats.overallCpuUsage": "979",
                "summary.hardware.memorySize": str(24 * 1024 ** 3), "summary.quickStats.overallMemoryUsage": "14465"}

    def test_host_scope_and_units(self):
        result = esxi.host_metrics(self.values())
        self.assertAlmostEqual(result["cpu_percent"], 12.2375)
        self.assertEqual(result["host_total_mib"], 24576)
        self.assertEqual(result["host_used_mib"], 14465)
        self.assertNotIn("password", result)

    def test_invalid_or_disconnected_statistics_are_not_zero(self):
        for key, value in (("runtime.connectionState", "disconnected"), ("summary.hardware.cpuMhz", "0"),
                           ("summary.quickStats.overallCpuUsage", None), ("summary.quickStats.overallMemoryUsage", "99999")):
            values = self.values()
            values[key] = value
            with self.assertRaises(esxi.ESXiError):
                esxi.host_metrics(values)

    def test_turbo_usage_keeps_raw_mhz_but_caps_protection_percentage(self):
        values = self.values()
        values["summary.quickStats.overallCpuUsage"] = "9000"
        result = esxi.host_metrics(values)
        self.assertEqual(result["cpu_percent"], 100)
        self.assertEqual(result["cpu_used_mhz"], 9000)

    def test_property_xml_namespaces(self):
        root = ET.fromstring('<Envelope xmlns="urn:test"><propSet><name>x</name><val>4</val></propSet></Envelope>')
        self.assertEqual(esxi.decode_properties(root), {"x": "4"})

    def test_certificate_pin_checked_before_api_requests(self):
        certificate = b"test certificate"
        fingerprint = hashlib.sha256(certificate).hexdigest()
        for pin, accepted in ((fingerprint, True), ("0" * 64, False)):
            connection = esxi.PinnedHTTPSConnection("example.test", 443, pin)
            connection.sock = mock.Mock()
            connection.sock.getpeercert.return_value = certificate
            with mock.patch.object(esxi.http.client.HTTPSConnection, "connect"), mock.patch.object(connection, "close") as close:
                if accepted:
                    connection.connect()
                else:
                    with self.assertRaises(esxi.ESXiError):
                        connection.connect()
                    close.assert_called_once()

    def test_atomic_public_metrics_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "metrics.json"
            if not hasattr(esxi.os, "fchmod"):
                self.skipTest("POSIX file modes required")
            esxi.write_atomic(output, esxi.host_metrics(self.values()))
            self.assertEqual(output.stat().st_mode & 0o777, 0o644)
            self.assertEqual(len(list(output.parent.iterdir())), 1)


if __name__ == "__main__":
    unittest.main()

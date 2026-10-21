#!/usr/bin/env python3
"""Poll read-only ESXi host statistics; publish no credentials to the portal."""

import argparse
import datetime
import hashlib
import hmac
import http.client
import http.cookies
import json
import os
import pathlib
import signal
import ssl
import tempfile
import threading
import urllib.parse
import xml.etree.ElementTree as ET
from xml.sax.saxutils import escape


class ESXiError(Exception):
    pass


class PinnedHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, port, fingerprint):
        super().__init__(host, port=port, timeout=5, context=ssl._create_unverified_context())
        self.fingerprint = fingerprint.lower().replace(":", "")

    def connect(self):
        super().connect()
        actual = hashlib.sha256(self.sock.getpeercert(binary_form=True)).hexdigest()
        if not hmac.compare_digest(actual, self.fingerprint):
            self.close()
            raise ESXiError("ESXi certificate fingerprint mismatch")


def localname(tag):
    return tag.rsplit("}", 1)[-1]


def child_text(root, name):
    return next((e.text for e in root.iter() if localname(e.tag) == name), None)


def decode_properties(root):
    values = {}
    for item in root.iter():
        if localname(item.tag) == "propSet":
            name = child_text(item, "name")
            values[name] = child_text(item, "val")
    return values


def host_metrics(values):
    if values.get("runtime.connectionState") != "connected":
        raise ESXiError("ESXi host is not connected")
    try:
        cores = int(values["summary.hardware.numCpuCores"])
        mhz = int(values["summary.hardware.cpuMhz"])
        used = int(values["summary.quickStats.overallCpuUsage"])
        total_memory = int(values["summary.hardware.memorySize"]) / 1048576
        used_memory = int(values["summary.quickStats.overallMemoryUsage"])
    except (KeyError, ValueError, TypeError):
        raise ESXiError("ESXi host statistics missing or invalid") from None
    if cores <= 0 or mhz <= 0 or used < 0 or total_memory <= 0 or not 0 <= used_memory <= total_memory:
        raise ESXiError("ESXi host statistics out of range")
    return {
        "source": "esxi",
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "cpu_count": cores,
        "cpu_used_mhz": used,
        "cpu_capacity_mhz": cores * mhz,
        "cpu_percent": min(100.0, 100.0 * used / (cores * mhz)),
        "host_used_mib": used_memory,
        "host_total_mib": total_memory,
    }


class Client:
    def __init__(self, config):
        parsed = urllib.parse.urlsplit(config["url"])
        if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.query or parsed.fragment:
            raise ESXiError("ESXi URL must be an HTTPS management address")
        self.config = config
        self.connection = PinnedHTTPSConnection(parsed.hostname, parsed.port or 443, config["certificate_sha256"])
        self.cookie = ""
        self.logged_in = False
        self.session_manager = ""
        self.property_collector = ""

    def call(self, method, body):
        envelope = ('<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">'
                    '<soapenv:Body><' + method + ' xmlns="urn:vim25">' + body + '</' + method + '></soapenv:Body></soapenv:Envelope>')
        headers = {"Content-Type": "text/xml; charset=utf-8", "SOAPAction": '"urn:vim25/6.0"'}
        if self.cookie:
            headers["Cookie"] = self.cookie
        self.connection.request("POST", "/sdk", envelope.encode(), headers)
        response = self.connection.getresponse()
        payload = response.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024:
            raise ESXiError("ESXi response too large")
        cookie = http.cookies.SimpleCookie()
        for name, value in response.getheaders():
            if name.lower() == "set-cookie":
                cookie.load(value)
        if "vmware_soap_session" in cookie:
            self.cookie = "vmware_soap_session=" + cookie["vmware_soap_session"].coded_value
        root = ET.fromstring(payload)
        if response.status != 200 or any(localname(e.tag) == "Fault" for e in root.iter()):
            raise ESXiError("ESXi API request failed: " + method)
        return root

    def login(self):
        content = self.call("RetrieveServiceContent", '<_this type="ServiceInstance">ServiceInstance</_this>')
        if child_text(content, "apiType") != "HostAgent":
            raise ESXiError("A direct ESXi host address is required")
        self.session_manager = child_text(content, "sessionManager")
        self.property_collector = child_text(content, "propertyCollector")
        if not self.session_manager or not self.property_collector:
            raise ESXiError("ESXi service references missing")
        self.call("Login", '<_this type="SessionManager">' + escape(self.session_manager) + '</_this><userName>'
                  + escape(self.config["username"]) + '</userName><password>' + escape(self.config["password"]) + '</password>')
        self.logged_in = True

    def collect(self):
        if not self.logged_in:
            self.login()
        paths = ("summary.hardware.numCpuCores", "summary.hardware.cpuMhz", "summary.hardware.memorySize",
                 "summary.quickStats.overallCpuUsage", "summary.quickStats.overallMemoryUsage", "runtime.connectionState")
        root = self.call("RetrievePropertiesEx", '<_this type="PropertyCollector">' + escape(self.property_collector)
                         + '</_this><specSet><propSet><type>HostSystem</type><all>false</all>'
                         + ''.join('<pathSet>' + p + '</pathSet>' for p in paths)
                         + '</propSet><objectSet><obj type="HostSystem">ha-host</obj></objectSet></specSet><options/>')
        return host_metrics(decode_properties(root))

    def close(self):
        if self.logged_in:
            try:
                self.call("Logout", '<_this type="SessionManager">' + escape(self.session_manager) + '</_this>')
            except Exception:
                pass
        self.logged_in = False
        self.connection.close()


def write_atomic(output, payload):
    fd, temporary = tempfile.mkstemp(prefix=".esxi-metrics-", dir=output.parent)
    try:
        os.fchmod(fd, 0o644)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(payload, stream, separators=(",", ":"), allow_nan=False)
            stream.write("\n")
        os.replace(temporary, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    config = json.loads(pathlib.Path(args.config).read_text())
    output = pathlib.Path(args.output)
    stopping = threading.Event()
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: stopping.set())
    client = Client(config)
    failed = False
    try:
        while not stopping.is_set():
            try:
                payload = client.collect()
                write_atomic(output, payload)
                if args.once:
                    print(json.dumps(payload, separators=(",", ":")))
                elif failed:
                    print("ESXi statistics collection recovered", flush=True)
                failed = False
            except Exception as error:
                if not failed:
                    print("ESXi statistics unavailable (" + type(error).__name__ + ")", flush=True)
                failed = True
                client.close()
                client = Client(config)
                if args.once:
                    raise SystemExit(1) from None
            if args.once:
                break
            stopping.wait(15 if failed else 5)
    finally:
        client.close()


if __name__ == "__main__":
    main()

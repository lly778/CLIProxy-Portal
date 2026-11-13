#!/usr/bin/env python3
"""Stage a narrowly scoped checkpoint-cap patch for Linux x86-64 AGY 1.5.3.

Default is read-only. --output writes a new executable, never the input.
This changes the checkpoint cap/fallback, not model metadata, tier selection,
input truncation, bridge limits, tool policy, or credentials. Updates require
fresh inspection; an unknown binary is rejected rather than pattern patched.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import struct


ORIGINAL_SHA256 = "7ca212748ca0dfda718efbc39a776a104472e7e9344d4b6ea147c543684f96a3"
PATCHES = (
    (0x8390982, bytes.fromhex("81 fa 00 e8 03 00"), bytes.fromhex("81 fa 00 d0 07 00")),
    (0x8390988, bytes.fromhex("be 00 e8 03 00"), bytes.fromhex("be 00 d0 07 00")),
    (0x8390CFA, bytes.fromhex("c7 40 0c 00 e8 03 00"), bytes.fromhex("c7 40 0c 00 d0 07 00")),
)


def file_offset(data, address):
    if data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 18)[0] != 62:
        raise ValueError("expected a little-endian x86-64 ELF")
    start = struct.unpack_from("<Q", data, 32)[0]
    size, count = struct.unpack_from("<HH", data, 54)
    for index in range(count):
        pos = start + index * size
        kind, flags, offset, va, _, length = struct.unpack_from("<IIQQQQ", data, pos)
        if kind == 1 and flags & 1 and va <= address < va + length:
            return offset + address - va
    raise ValueError("instruction address is not in executable file data")


def patched(data):
    digest = hashlib.sha256(data).hexdigest()
    result = bytearray(data)
    # The three full instructions and following unsigned minimum are pinned.
    for address, old, new in PATCHES:
        pos = file_offset(data, address)
        if data[pos:pos + len(old)] != old:
            raise ValueError("unexpected instruction at " + hex(address))
        result[pos:pos + len(old)] = new
    pos = file_offset(data, 0x839098D)
    if data[pos:pos + 3] != bytes.fromhex("0f 42 f2"):
        raise ValueError("model-cap minimum instruction changed")
    if digest != ORIGINAL_SHA256:
        raise ValueError("unsupported binary checksum; inspect the new version first")
    if len(result) != len(data) or sum(a != b for a, b in zip(data, result)) != 6:
        raise ValueError("unexpected patch size")
    return bytes(result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    source = args.binary.resolve(strict=True)
    data = source.read_bytes()
    result = patched(data)
    # Validate the actual patched unsigned min instruction on Linux x86-64,
    # without starting AGY or making a provider request.
    import ctypes
    import mmap
    import platform
    checked = False
    if platform.system() == "Linux" and platform.machine() == "x86_64":
        code = bytes.fromhex("89 fa") + result[file_offset(result, 0x8390982):file_offset(result, 0x8390990)] + bytes.fromhex("89 f0 c3")
        with mmap.mmap(-1, len(code), prot=mmap.PROT_READ | mmap.PROT_WRITE | mmap.PROT_EXEC) as mem:
            mem.write(code)
            fn = ctypes.CFUNCTYPE(ctypes.c_uint32, ctypes.c_uint32)(ctypes.addressof(ctypes.c_char.from_buffer(mem)))
            for value in (0, 64000, 131072, 256000, 511999, 512000, 512001, 983040, 1048576):
                if fn(value) != min(value, 512000):
                    raise ValueError("patched minimum boundary check failed")
        checked = True
    if args.output:
        target = args.output.resolve()
        if target == source:
            raise ValueError("in-place writes are not supported; stage then atomically replace")
        with target.open("xb") as stream:
            stream.write(result)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(target, source.stat().st_mode & 0o777)
    print(json.dumps({"original_sha256": ORIGINAL_SHA256, "patched_sha256": hashlib.sha256(result).hexdigest(), "checkpoint_cap_tokens": 512000, "modified_bytes": 6, "minimum_boundary_checks": checked, "staged": bool(args.output)}))


if __name__ == "__main__":
    main()

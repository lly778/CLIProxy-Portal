"""Bounded Linux TCP-header observer. No payload or pcap is retained.

Run as root via SSH. JSON lines on stdin set labels or stop the observation.
Only new IPv4 TCP connections involving the configured server/ports are kept.
"""
import argparse
import json
import socket
import struct
import sys
import threading
import time


def decode_header(packet):
    """Parse a truncated Ethernet/IPv4/TCP snapshot, using IP length for payload size."""
    if len(packet) < 54 or packet[12:14] != b"\x08\x00":
        return None
    ip = 14
    ihl = (packet[ip] & 15) * 4
    if packet[ip] >> 4 != 4 or ihl < 20 or packet[ip + 9] != 6:
        return None
    # Ignore fragmented packets rather than interpreting fragment data as TCP.
    if struct.unpack_from("!H", packet, ip + 6)[0] & 0x3FFF:
        return None
    tcp = ip + ihl
    if len(packet) < tcp + 20:
        return None
    source = socket.inet_ntoa(packet[ip + 12:ip + 16])
    destination = socket.inet_ntoa(packet[ip + 16:ip + 20])
    sport, dport, seq, ack = struct.unpack_from("!HHII", packet, tcp)
    thl = (packet[tcp + 12] >> 4) * 4
    if thl < 20 or len(packet) < tcp + thl:
        return None
    total = struct.unpack_from("!H", packet, ip + 2)[0]
    if total < ihl + thl:
        return None
    options = packet[tcp + 20:tcp + thl]
    mss = scale = None
    sack_blocks = []
    index = 0
    while index < len(options):
        kind = options[index]
        if kind == 0:
            break
        if kind == 1:
            index += 1
            continue
        if index + 1 >= len(options):
            break
        length = options[index + 1]
        if length < 2 or index + length > len(options):
            break
        value = options[index + 2:index + length]
        if kind == 2 and len(value) == 2:
            mss = struct.unpack("!H", value)[0]
        elif kind == 3 and len(value) == 1:
            scale = value[0]
        elif kind == 5 and len(value) % 8 == 0:
            sack_blocks = [struct.unpack_from("!II", value, pos) for pos in range(0, len(value), 8)]
        index += length
    return dict(source=source, destination=destination, sport=sport, dport=dport,
                seq=seq, ack=ack, flags=packet[tcp + 13],
                window=struct.unpack_from("!H", packet, tcp + 14)[0],
                data_bytes=total - ihl - thl, mss=mss, window_scale=scale,
                sack_blocks=sack_blocks)


def add_interval(intervals, start, end):
    """Return overlap bytes, merging observed sequence ranges (resegmentation safe)."""
    overlap = sum(max(0, min(end, right) - max(start, left)) for left, right in intervals)
    intervals.append((start, end))
    intervals.sort()
    merged = []
    for left, right in intervals:
        if merged and left <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(right, merged[-1][1]))
        else:
            merged.append((left, right))
    intervals[:] = merged
    return overlap


def flow_summary(flow):
    events = flow["events"]
    summary = dict(label=flow["label"], client_ip=flow["client_ip"],
                   client_port=flow["client_port"], server_port=flow["server_port"],
                   syn_count=0, synack_count=0, handshake_ms=None,
                   client_rst=0, server_rst=0, directions={})
    first_syn = None
    for event in events:
        flags = event["flags"]
        if event["direction"] == "client" and flags & 2 and not flags & 16:
            summary["syn_count"] += 1
            if first_syn is None:
                first_syn = event["at_ms"]
        if event["direction"] == "server" and flags & 2 and flags & 16:
            summary["synack_count"] += 1
        if summary["handshake_ms"] is None and first_syn is not None and event["direction"] == "client" and flags & 16 and not flags & 2:
            summary["handshake_ms"] = round(event["at_ms"] - first_syn, 3)
        if flags & 4:
            summary[event["direction"] + "_rst"] += 1
    for direction in ["client", "server"]:
        selected = [event for event in events if event["direction"] == direction]
        intervals = []
        data_events = []
        overlap_bytes = overlap_packets = 0
        base = next((event["seq"] for event in selected if event["flags"] & 2), None)
        highwater = 0
        behind_highwater = 0
        for event in selected:
            if not event["data_bytes"] or base is None:
                continue
            relative = (event["seq"] - base) & 0xFFFFFFFF
            if relative < highwater:
                behind_highwater += 1
            highwater = max(highwater, relative + event["data_bytes"])
            overlap = add_interval(intervals, relative, relative + event["data_bytes"])
            overlap_bytes += overlap
            overlap_packets += int(overlap > 0)
            data_events.append(event)
        gaps = [right["at_ms"] - left["at_ms"] for left, right in zip(data_events, data_events[1:])]
        syn_event = next((event for event in selected if event["flags"] & 2), {})
        scale = syn_event.get("window_scale") or 0
        windows = [event["window"] << scale for event in selected if event["flags"] & 16 and not event["flags"] & 6]
        summary["directions"][direction] = dict(
            packets=len(selected), data_packets=len(data_events),
            observed_data_bytes=sum(event["data_bytes"] for event in data_events),
            unique_sequence_bytes=sum(right - left for left, right in intervals),
            overlap_packets=overlap_packets, overlap_bytes=overlap_bytes,
            behind_highwater_packets=behind_highwater,
            largest_data_gaps_ms=sorted((round(gap, 3) for gap in gaps), reverse=True)[:5],
            first_data_ms=data_events[0]["at_ms"] if data_events else None,
            last_data_ms=data_events[-1]["at_ms"] if data_events else None,
            sack_packets=sum(bool(event["sack_blocks"]) for event in selected),
            zero_window_acks=sum(window == 0 for window in windows),
            smallest_advertised_window_bytes=min(windows) if windows else None,
            advertised_mss=syn_event.get("mss"), window_scale=syn_event.get("window_scale"))
    return summary


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--server-ip", default="212.135.210.56")
    parser.add_argument("--interface", default="eth0")
    parser.add_argument("--seconds", type=int, default=360)
    args = parser.parse_args()
    stop = threading.Event()
    state = {"label": "unlabelled"}
    output_lock = threading.Lock()

    def emit(value):
        with output_lock:
            print(json.dumps(value), flush=True)

    def control():
        for line in sys.stdin:
            command = json.loads(line)
            if command.get("stop"):
                stop.set()
                break
            state["label"] = command["label"]
            emit({"label_ack": state["label"], "server_time": time.time()})
        stop.set()

    # ETH_P_ALL is necessary to observe outbound packets as well as ingress.
    raw = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003))
    raw.bind((args.interface, 0))
    raw.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 * 1024 * 1024)
    raw.settimeout(0.25)
    start = time.monotonic()
    flows = {}
    emit({"ready": True, "server_time": time.time(), "payload_retained": False})
    threading.Thread(target=control, daemon=True).start()
    try:
        while not stop.is_set() and time.monotonic() - start < args.seconds:
            try:
                # Enough for Ethernet + maximum IPv4 and TCP headers. Never save raw bytes.
                packet, address = raw.recvfrom(160)
            except socket.timeout:
                continue
            event = decode_header(packet)
            if not event:
                continue
            if event["destination"] == args.server_ip and event["dport"] in (443, 8317):
                direction = "client"
                key = (event["source"], event["sport"], event["dport"])
            elif event["source"] == args.server_ip and event["sport"] in (443, 8317):
                direction = "server"
                key = (event["destination"], event["dport"], event["sport"])
            else:
                continue
            if key not in flows:
                if direction != "client" or not event["flags"] & 2 or event["flags"] & 16:
                    continue
                flows[key] = dict(label=state["label"], client_ip=key[0], client_port=key[1], server_port=key[2], events=[])
            event["direction"] = direction
            event["packet_type"] = address[2]
            event["at_ms"] = round((time.monotonic() - start) * 1000, 3)
            flows[key]["events"].append(event)
    finally:
        # Linux PACKET_STATISTICS reports observer queue drops, not network drops.
        observed, dropped = struct.unpack("II", raw.getsockopt(263, 6, 8))
        raw.close()
    emit({"capture": {"seconds": round(time.monotonic() - start, 3),
                      "observer_packets": observed, "observer_queue_drops": dropped,
                      "flows": list(flows.values()),
                      "summaries": [flow_summary(flow) for flow in flows.values()],
                      "notes": "Header-only, IPv4, new flows only. Labels mark time periods, not exclusive attribution. Background connections may overlap. Overlap is not proof of where loss occurred. NIC offload can coalesce segments."}})


if __name__ == "__main__":
    main()

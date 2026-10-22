package ops

import "embed"

// Host assets are part of the signed/built portal program. Restore never
// executes systemd units or Python programs supplied by an uploaded archive.
//
//go:embed collect-host-log-metrics.py collect-esxi-metrics.py cliproxy.slice cliproxy-portal-log-metrics.service cliproxy-portal-log-metrics.timer resources/cliproxy-esxi-metrics.service backup/cliproxy-portal-backup.service backup/cliproxy-portal-backup.timer backup/restore.example.json backup/sources.example.json backup/upstream.approved.yaml
var HostAssets embed.FS

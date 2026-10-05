package backup

// New backups use these names consistently in the archive and GitHub release.
const (
	archiveName      = "backup.cbackup"
	manifestKind     = "backup"
	releaseMarker    = "CLIProxy Portal backup v1"
	releaseTagPrefix = "portal-backup-"
)

// Read-only identifiers for backups already published by older versions.
// Never emit these names for a new backup; removing them would strand old data.
const (
	legacyArchiveName      = "lightweight.cbackup"
	legacyManifestKind     = "lightweight"
	legacyReleaseMarker    = "CLIProxy Portal lightweight backup v1"
	legacyReleaseTagPrefix = "portal-light-"
)

func supportedManifestKind(kind string) bool {
	return kind == manifestKind || kind == legacyManifestKind
}

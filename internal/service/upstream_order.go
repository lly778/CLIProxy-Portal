package service

import "strings"

// UpstreamChannelLess keeps channel groups consistent across pages and pickers.
// Accept both routing keys and display labels used by quota projections.
func UpstreamChannelLess(left, right string) bool {
	key := func(value string) (int, string) {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "codex":
			return 0, "codex"
		case "antigravity":
			return 1, "antigravity"
		case "agy", "antigravity cli（acp）":
			return 2, "agy"
		default:
			return 3, value
		}
	}
	lRank, lKey := key(left)
	rRank, rKey := key(right)
	if lRank != rRank {
		return lRank < rRank
	}
	return lKey < rKey
}

// ModelIDLess keeps the flat CPA catalog before the ACP namespace, with names
// sorted within each catalog. No model discovery is needed just for ordering.
func ModelIDLess(left, right string) bool {
	left, right = strings.ToLower(left), strings.ToLower(right)
	lACP, rACP := strings.HasPrefix(left, "agy/"), strings.HasPrefix(right, "agy/")
	if lACP != rACP {
		return !lACP
	}
	return left < right
}

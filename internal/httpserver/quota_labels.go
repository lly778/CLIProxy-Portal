package httpserver

import (
	"cliproxy-portal/internal/webui"
	"strings"
)

// Account rows use the original compact Codex periods; shared pool titles keep
// their descriptive labels. Other providers retain their model-group labels.
func upstreamAccountQuotaLabel(channel, label, period string) string {
	if channel == "codex" {
		switch period {
		case "five_hour":
			return "5H"
		case "monthly":
			return "月"
		default:
			return "7D"
		}
	}
	display := quotaDisplayLabel(label, "", period)
	return strings.NewReplacer("5 小时额度", "5H", "周额度", "7D", "月额度", "月").Replace(display)
}

// Group only explicitly labeled windows. Keep unknown/model-specific quota
// formats as flat rows rather than inferring a period or merging their values.
func groupAccountQuotaViews(quotas []webui.UpstreamAccountQuotaView) []webui.UpstreamAccountQuotaGroupView {
	var groups []webui.UpstreamAccountQuotaGroupView
	indices := map[string]int{}
	for _, quota := range quotas {
		at := strings.LastIndex(quota.Label, " · ")
		if at < 0 {
			return nil
		}
		name, period := quota.Label[:at], quota.Label[at+len(" · "):]
		if name == "" || (period != "5H" && period != "7D" && period != "月") {
			return nil
		}
		index, found := indices[name]
		if !found {
			index = len(groups)
			indices[name] = index
			groups = append(groups, webui.UpstreamAccountQuotaGroupView{Label: name})
		}
		quota.Label = period
		groups[index].Quotas = append(groups[index].Quotas, quota)
	}
	return groups
}

// Separate model families for presentation only; never combine their quotas.
// Unrecognized labels keep the original flat layout.
func groupPoolQuotaViews(quotas []webui.QuotaGroupView) [][]webui.QuotaGroupView {
	if len(quotas) <= 2 {
		return nil
	}
	var columns [][]webui.QuotaGroupView
	indices := map[string]int{}
	for _, quota := range quotas {
		at := strings.LastIndex(quota.Label, " · ")
		if at < 1 {
			return nil
		}
		name, period := quota.Label[:at], quota.Label[at+len(" · "):]
		if period != "5 小时额度" && period != "周额度" && period != "月额度" {
			return nil
		}
		index, exists := indices[name]
		if !exists {
			index = len(columns)
			indices[name] = index
			columns = append(columns, nil)
		}
		columns[index] = append(columns[index], quota)
	}
	if len(columns) < 2 {
		return nil
	}
	for _, column := range columns {
		if len(column) > 2 {
			return nil
		}
	}
	return columns
}

// Keep provider IDs and grouping keys untouched; localize only display text.
func quotaDisplayLabel(label, plan, period string) string {
	if label = strings.TrimSpace(label); label != "" {
		parts := strings.Split(label, " · ")
		for i, part := range parts {
			switch strings.ToLower(strings.TrimSpace(part)) {
			case "claude and gpt models", "claude/gpt", "claude / gpt":
				parts[i] = "Claude / GPT"
			case "gemini models", "gemini":
				parts[i] = "Gemini"
			case "five hour limit remaining", "five hour", "5h", "5 hour":
				parts[i] = "5 小时额度"
			case "weekly limit remaining", "weekly", "7d":
				parts[i] = "周额度"
			case "monthly limit remaining", "monthly":
				parts[i] = "月额度"
			}
		}
		return strings.Join(parts, " · ")
	}
	name := "周额度"
	switch period {
	case "five_hour":
		name = "5 小时额度"
	case "monthly":
		name = "月额度"
	}
	if plan = strings.TrimSpace(plan); plan != "" && !strings.EqualFold(plan, "unknown") {
		return strings.ToUpper(plan) + " · " + name
	}
	return name
}

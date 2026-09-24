package httpserver

import (
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
)

func auditActionDisplay(action string) string {
	switch action {
	case "admin.create":
		return "创建管理员"
	case "admin.promote":
		return "设为管理员"
	case "admin.demote":
		return "取消管理员"
	case "admin.disable":
		return "停用管理员"
	case "gateway.dialogue.download":
		return "下载交互记录"
	case "key.issue":
		return "领取接口密钥"
	case "key.revoke":
		return "撤销接口密钥"
	case "key.revoke.pending":
		return "等待撤销接口密钥"
	case "key.test":
		return "检查接口密钥"
	case "key.test.external_revoked":
		return "发现接口密钥已撤销"
	case "key.model_test":
		return "测试模型连接"
	case "key.model_test.failed":
		return "模型连接测试失败"
	case "oauth_model.aliases.update":
		return "修改模型别名"
	case "oauth_model.reasoning_caps.update":
		return "修改思考上限"
	case "oauth_model.enable":
		return "启用模型"
	case "oauth_model.disable":
		return "停用模型"
	case "quota.refresh":
		return "刷新额度"
	case "upstream.enable":
		return "启用上游账号"
	case "upstream.disable":
		return "停用上游账号"
	case "user.register":
		return "提交注册申请"
	case "user.resubmit":
		return "重新提交注册申请"
	case "user.approve":
		return "批准注册申请"
	case "user.reject":
		return "拒绝注册申请"
	case "user.password.change":
		return "修改密码"
	case "user.password.reset_issued":
		return "签发密码重置码"
	case "user.password.reset":
		return "重置密码"
	case "user.phone.change":
		return "修改手机号"
	case "user.suspend":
		return "停用用户"
	case "user.suspend.pending":
		return "等待停用用户"
	case "user.unsuspend":
		return "恢复用户"
	case "user.delete":
		return "删除用户"
	case "user.delete.pending":
		return "等待删除用户"
	case "policy.publish":
		return "发布使用规则"
	case "registration.toggle":
		return "修改注册设置"
	default:
		return "其他操作"
	}
}

func auditTargetDisplay(action, id, label string) string {
	target := strings.TrimSpace(label)
	if target == "" {
		target = strings.TrimSpace(id)
	}
	switch action {
	case "registration.toggle":
		return "注册设置"
	case "policy.publish":
		if target != "" {
			return "使用规则第 " + target + " 版"
		}
	case "quota.refresh":
		if strings.EqualFold(target, "codex") {
			return "Codex 额度"
		}
	case "oauth_model.aliases.update", "oauth_model.reasoning_caps.update":
		if strings.EqualFold(target, "codex") {
			return "Codex 模型"
		}
	case "gateway.dialogue.download":
		if target != "" {
			return "交互记录（" + target + "）"
		}
	case "upstream.enable", "upstream.disable":
		if target != "" {
			return "上游账号（" + target + "）"
		}
	case "key.issue", "key.revoke.pending":
		if target != "" {
			return "接口密钥（" + target + "）"
		}
	case "key.test", "key.test.external_revoked", "key.revoke":
		if target != "" && target == id {
			return "用户（" + target + "）"
		}
	}
	if (strings.HasPrefix(action, "user.") || strings.HasPrefix(action, "admin.")) && target != "" && target == id {
		return "用户（" + target + "）"
	}
	if target == "" {
		return "—"
	}
	return target
}

func auditDetailDisplay(action, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "—"
	}
	switch action {
	case "admin.promote":
		if detail == "user -> admin" {
			return "普通用户转为管理员"
		}
	case "admin.demote":
		if detail == "admin -> user" {
			return "管理员转为普通用户"
		}
	case "key.issue":
		if detail == "claim" {
			return "用户领取密钥"
		}
	case "key.revoke":
		if detail == "API Key 已撤销" {
			return "接口密钥已撤销"
		}
	case "key.model_test":
		if duration, ok := strings.CutPrefix(detail, "latency="); ok {
			return "耗时 " + auditDurationDisplay(duration)
		}
	case "key.model_test.failed":
		if detail != "" {
			return "错误：" + detail
		}
	case "oauth_model.aliases.update", "oauth_model.reasoning_caps.update":
		if detail == "Codex" {
			return "旧记录未保存变更明细"
		}
		if action == "oauth_model.reasoning_caps.update" {
			return auditReasoningDetailDisplay(detail)
		}
	case "oauth_model.enable", "oauth_model.disable":
		if detail == "Codex" {
			return "Codex 模型"
		}
	case "registration.toggle":
		if detail == "true" {
			return "开放注册"
		}
		if detail == "false" {
			return "关闭注册"
		}
	case "upstream.enable", "upstream.disable":
		if detail != "" {
			return "服务商：" + detail
		}
	case "user.phone.change":
		return strings.ReplaceAll(detail, " -> ", " → ")
	}
	return detail
}

func auditDurationDisplay(value string) string {
	for _, unit := range []struct{ suffix, label string }{{"ms", "毫秒"}, {"µs", "微秒"}, {"s", "秒"}} {
		if amount, ok := strings.CutSuffix(value, unit.suffix); ok {
			return amount + " " + unit.label
		}
	}
	return value
}

func auditReasoningDetailDisplay(detail string) string {
	changes := strings.Split(detail, "；")
	for index, change := range changes {
		model, values, ok := strings.Cut(change, ": ")
		if !ok {
			continue
		}
		before, after, ok := strings.Cut(values, " → ")
		if !ok {
			continue
		}
		changes[index] = model + ": " + reasoningCapLabel(before) + " → " + reasoningCapLabel(after)
	}
	return strings.Join(changes, "；")
}

func aliasChangeDetails(before, after []cpamp.OAuthModelAlias) string {
	oldSettings := aliasSettings(before)
	newSettings := aliasSettings(after)
	ids := make(map[string]bool, len(oldSettings)+len(newSettings))
	for id := range oldSettings {
		ids[id] = true
	}
	for id := range newSettings {
		ids[id] = true
	}
	models := make([]string, 0, len(ids))
	for id := range ids {
		models = append(models, id)
	}
	sort.Strings(models)
	changes := make([]string, 0, len(models))
	for _, id := range models {
		oldValue := oldSettings[id]
		newValue := newSettings[id]
		if oldValue != newValue {
			changes = append(changes, id+": "+aliasSettingLabel(oldValue)+" → "+aliasSettingLabel(newValue))
		}
	}
	if len(changes) == 0 {
		return "配置未变化"
	}
	return strings.Join(changes, "；")
}

func aliasSettingLabel(value string) string {
	if value == "" {
		return "原名"
	}
	return value
}

func aliasSettings(aliases []cpamp.OAuthModelAlias) map[string]string {
	type selection struct {
		aliases      []string
		keepOriginal bool
	}
	grouped := make(map[string]*selection)
	for _, alias := range aliases {
		id := strings.TrimSpace(alias.Name)
		name := strings.TrimSpace(alias.Alias)
		if id == "" || name == "" {
			continue
		}
		entry := grouped[id]
		if entry == nil {
			entry = &selection{}
			grouped[id] = entry
		}
		entry.aliases = append(entry.aliases, name)
		entry.keepOriginal = entry.keepOriginal || alias.Fork
	}
	settings := make(map[string]string, len(grouped))
	for id, entry := range grouped {
		sort.Strings(entry.aliases)
		value := strings.Join(entry.aliases, ", ")
		if entry.keepOriginal {
			value += "（保留原名）"
		}
		settings[id] = value
	}
	return settings
}

func reasoningCapChangeDetails(before, after map[string]string) string {
	ids := make(map[string]bool, len(before)+len(after))
	for id := range before {
		ids[id] = true
	}
	for id := range after {
		ids[id] = true
	}
	models := make([]string, 0, len(ids))
	for id := range ids {
		models = append(models, id)
	}
	sort.Strings(models)
	changes := make([]string, 0, len(models))
	for _, id := range models {
		if before[id] != after[id] {
			changes = append(changes, id+": "+reasoningCapLabel(before[id])+" → "+reasoningCapLabel(after[id]))
		}
	}
	if len(changes) == 0 {
		return "配置未变化"
	}
	return strings.Join(changes, "；")
}

func reasoningCapLabel(value string) string {
	if value == "" {
		return "不限制"
	}
	if label := reasoningEffortLabel(value); label != value {
		return label
	}
	return value
}

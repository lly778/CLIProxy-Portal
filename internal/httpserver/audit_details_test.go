package httpserver

import (
	"context"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

func TestAuditActionDisplay(t *testing.T) {
	cases := map[string]string{
		"admin.create": "创建管理员", "admin.promote": "设为管理员", "admin.demote": "取消管理员", "admin.disable": "停用管理员",
		"gateway.dialogue.download": "下载交互记录",
		"key.issue":                 "领取接口密钥", "key.revoke": "撤销接口密钥", "key.revoke.pending": "等待撤销接口密钥",
		"key.test": "检查接口密钥", "key.test.external_revoked": "发现接口密钥已撤销",
		"key.model_test": "测试模型连接", "key.model_test.failed": "模型连接测试失败",
		"oauth_model.aliases.update": "修改模型别名", "oauth_model.reasoning_caps.update": "修改思考上限",
		"oauth_model.enable": "启用模型", "oauth_model.disable": "停用模型",
		"quota.refresh": "刷新额度", "upstream.enable": "启用上游账号", "upstream.disable": "停用上游账号",
		"user.register": "提交注册申请", "user.resubmit": "重新提交注册申请", "user.approve": "批准注册申请",
		"user.reject": "拒绝注册申请", "user.password.change": "修改密码", "user.password.reset_issued": "签发密码重置码",
		"user.password.reset": "重置密码", "user.phone.change": "修改手机号", "user.suspend": "停用用户",
		"user.suspend.pending": "等待停用用户", "user.unsuspend": "恢复用户", "user.delete": "删除用户",
		"user.delete.pending": "等待删除用户", "policy.publish": "发布使用规则", "registration.toggle": "修改注册设置",
	}
	for action, want := range cases {
		if got := auditActionDisplay(action); got != want {
			t.Errorf("action %q = %q, want %q", action, got, want)
		}
	}
	if got := auditActionDisplay("future.action"); got != "其他操作" {
		t.Fatalf("unknown action = %q", got)
	}
}

func TestAuditTargetAndDetailDisplay(t *testing.T) {
	cases := []struct {
		action, id, label, detail, targetWant, detailWant string
	}{
		{"quota.refresh", "codex", "codex", "用户手动刷新", "Codex 额度", "用户手动刷新"},
		{"gateway.dialogue.download", "record-1", "record-1", "用户与助手文本及实际工具交互", "交互记录（record-1）", "用户与助手文本及实际工具交互"},
		{"key.issue", "key-1", "key-1", "claim", "接口密钥（key-1）", "用户领取密钥"},
		{"key.model_test", "gpt-6-luna", "gpt-6-luna", "latency=123ms", "gpt-6-luna", "耗时 123 毫秒"},
		{"upstream.enable", "account-1", "account-1", "openai", "上游账号（account-1）", "服务商：openai"},
		{"registration.toggle", "", "", "false", "注册设置", "关闭注册"},
		{"policy.publish", "3", "3", "标题", "使用规则第 3 版", "标题"},
		{"admin.promote", "user-1", "user-1", "user -> admin", "用户（user-1）", "普通用户转为管理员"},
		{"user.approve", "user-1", "小王(138****1234)", "", "小王(138****1234)", "—"},
		{"oauth_model.reasoning_caps.update", "codex", "codex", "gpt-6-luna: high → max", "Codex 模型", "gpt-6-luna: 高 → 最高"},
	}
	for _, tc := range cases {
		if got := auditTargetDisplay(tc.action, tc.id, tc.label); got != tc.targetWant {
			t.Errorf("target %q = %q, want %q", tc.action, got, tc.targetWant)
		}
		if got := auditDetailDisplay(tc.action, tc.detail); got != tc.detailWant {
			t.Errorf("detail %q = %q, want %q", tc.action, got, tc.detailWant)
		}
	}
}

func TestAuditViewsExplainLegacyModelChanges(t *testing.T) {
	s := &Server{}
	items := []domain.AuditEvent{{Action: "oauth_model.aliases.update", TargetLabel: "codex", Detail: "Codex"}}
	views := s.auditViews(context.Background(), items)
	if len(views) != 1 || views[0].Action != "修改模型别名" || views[0].Target != "Codex 模型" || views[0].Details != "旧记录未保存变更明细" {
		t.Fatalf("legacy audit view = %+v", views)
	}
}

func TestAliasChangeDetails(t *testing.T) {
	before := []cpamp.OAuthModelAlias{{Name: "gpt-6-luna", Alias: "old", Fork: true}}
	after := []cpamp.OAuthModelAlias{{Name: "gpt-6-luna", Alias: "new", Fork: false}}
	got := aliasChangeDetails(before, after)
	if !strings.Contains(got, "gpt-6-luna: old（保留原名） → new") {
		t.Fatalf("alias change = %q", got)
	}
	if got := aliasChangeDetails(nil, nil); got != "配置未变化" {
		t.Fatalf("unchanged alias detail = %q", got)
	}
}

func TestReasoningCapChangeDetails(t *testing.T) {
	got := reasoningCapChangeDetails(map[string]string{"gpt-6-luna": "high"}, map[string]string{"gpt-6-luna": ""})
	if got != "gpt-6-luna: 高 → 不限制" {
		t.Fatalf("reasoning cap change = %q", got)
	}
}

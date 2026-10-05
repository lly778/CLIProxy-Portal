package service

import (
	"reflect"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
	"gopkg.in/yaml.v3"
)

func antiReasoningFixture(t *testing.T) (*channelCPAMP, *Keys) {
	t.Helper()
	f := newChannelCPAMP()
	f.models["codex"] = []cpamp.OAuthModelDefinition{{ID: "shared", ThinkingLevels: []string{"low", "medium", "high"}}}
	f.models["antigravity"] = []cpamp.OAuthModelDefinition{{ID: "shared", ThinkingLevels: []string{"low", "medium", "high"}, ThinkingMin: 128, ThinkingMax: 32768}, {ID: "claude-budget", ThinkingMin: 1024, ThinkingMax: 64000, ThinkingZeroAllowed: true}}
	return f, NewKeys(nil, f)
}

func TestAntigravityReasoningCapabilities(t *testing.T) {
	budget := OAuthModelSetting{ThinkingMin: 1024, ThinkingMax: 64000, ThinkingZeroAllowed: true}
	if got := OAuthReasoningLevels("antigravity", budget); !reflect.DeepEqual(got, []string{"none", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("levels %v", got)
	}
	if n, ok := OAuthReasoningBudget("antigravity", budget, "max"); !ok || n != 64000 {
		t.Fatalf("max=%d ok=%v", n, ok)
	}
	if len(OAuthReasoningLevels("antigravity", OAuthModelSetting{})) != 0 {
		t.Fatal("unsupported model received invented capability")
	}
	gemini := OAuthModelSetting{ThinkingLevels: []string{"minimal", "high"}, ThinkingMin: 128, ThinkingMax: 32768}
	if got := OAuthReasoningLevels("antigravity", gemini); !reflect.DeepEqual(got, []string{"minimal", "high"}) {
		t.Fatalf("declared levels %v", got)
	}
}

func TestAntigravityReasoningCapsAndCodexAreIsolated(t *testing.T) {
	f, k := antiReasoningFixture(t)
	_, revision, err := k.OAuthReasoningCaps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(t.Context(), []ReasoningCapInput{{Model: "shared", Cap: "high"}}, revision); err != nil {
		t.Fatal(err)
	}
	_, revision, err = k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	inputs := []ReasoningCapInput{{Model: "shared", Cap: "low"}, {Model: "claude-budget", Cap: "medium"}}
	if err := k.SetOAuthReasoningCaps(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	codex, _, err := k.OAuthReasoningCaps(t.Context())
	if err != nil || codex["shared"] != "high" {
		t.Fatalf("codex=%v err=%v", codex, err)
	}
	anti, revision, err := k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || anti["shared"] != "low" || anti["claude-budget"] != "medium" {
		t.Fatalf("anti=%v err=%v", anti, err)
	}
	_, rules, err := parseReasoningConfig(f.config)
	if err != nil {
		t.Fatal(err)
	}
	_, owned, err := parseManagedReasoningGroupsForChannel("antigravity", rules)
	if err != nil {
		t.Fatal(err)
	}
	for index := range owned {
		models, _ := yamlField(rules.Content[index], "models")
		selector := models.Content[0]
		protocol, _ := yamlField(selector, "protocol")
		if protocol.Value != "antigravity" {
			t.Fatal("foreign protocol rule")
		}
		params, _ := yamlField(rules.Content[index], "params")
		if strings.HasSuffix(params.Content[0].Value, "thinkingBudget") {
			exists, _ := yamlField(selector, "exist")
			want := "generationConfig.thinkingConfig.@this|[thinkingBudget].#(>" + params.Content[1].Value + ")"
			if exists == nil || len(exists.Content) != 1 || exists.Content[0].Value != want {
				t.Fatal("budget cap must only match excessive numeric values")
			}
		} else {
			match, _ := yamlField(selector, "match")
			if match == nil {
				t.Fatal("level cap must be conditional")
			}
		}
	}
	puts := f.configWrites
	if err := k.SetOAuthReasoningCaps(t.Context(), inputs, revision, "antigravity"); err != nil || f.configWrites != puts {
		t.Fatalf("no-op err=%v writes=%d", err, f.configWrites)
	}
	inputs[0].Cap = ""
	inputs[1].Cap = ""
	if err := k.SetOAuthReasoningCaps(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	anti, _, err = k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || len(anti) != 0 {
		t.Fatalf("clear=%v err=%v", anti, err)
	}
	codex, _, err = k.OAuthReasoningCaps(t.Context())
	if err != nil || codex["shared"] != "high" {
		t.Fatal("clearing Antigravity changed Codex")
	}
}

func TestAntigravityReasoningCommentRewriteAndTampering(t *testing.T) {
	f, k := antiReasoningFixture(t)
	_, revision, _ := k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err := k.SetOAuthReasoningCaps(t.Context(), []ReasoningCapInput{{Model: "shared", Cap: "low"}, {Model: "claude-budget", Cap: "max"}}, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	doc, rules, _ := parseReasoningConfig(f.config)
	for _, rule := range rules.Content {
		models, _ := yamlField(rule, "models")
		name, _ := yamlField(models.Content[0], "name")
		name.LineComment = ""
		rule.HeadComment = ""
	}
	f.config, _ = yaml.Marshal(doc)
	caps, _, err := k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || caps["claude-budget"] != "max" {
		t.Fatalf("rewritten=%v err=%v", caps, err)
	}
	// A model maximum change updates the actual numeric ceiling, even if the
	// selected level and the generated rule count have not changed.
	f.models["antigravity"][1].ThinkingMax = 48000
	_, revision, _ = k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err := k.SetOAuthReasoningCaps(t.Context(), []ReasoningCapInput{{Model: "shared", Cap: "low"}, {Model: "claude-budget", Cap: "max"}}, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.config), "#(>48000)") {
		t.Fatal("stale model maximum retained")
	}
	doc, rules, _ = parseReasoningConfig(f.config)
	for _, rule := range rules.Content {
		models, _ := yamlField(rule, "models")
		name, _ := yamlField(models.Content[0], "name")
		if strings.Contains(name.LineComment, antigravityReasoningMarker) {
			protocol, _ := yamlField(models.Content[0], "protocol")
			protocol.Value = "codex"
			break
		}
	}
	f.config, _ = yaml.Marshal(doc)
	if _, _, err := k.OAuthReasoningCaps(t.Context(), "antigravity"); err == nil {
		t.Fatal("tampered owned rule accepted")
	}
	if _, _, err := k.OAuthReasoningCaps(t.Context()); err == nil {
		t.Fatal("foreign malformed owned rules should block writes too")
	}
}

func TestAntigravityReasoningPresetRoundTrip(t *testing.T) {
	f, k := antiReasoningFixture(t)
	_, revision, _ := k.OAuthReasoningCaps(t.Context(), "antigravity")
	inputs := []ReasoningCapInput{{Model: "shared", Cap: "medium"}, {Model: "claude-budget", Cap: "high"}}
	if err := k.SetOAuthReasoningCaps(t.Context(), inputs, revision, "antigravity"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Channels[1].ReasoningCaps) != 2 {
		t.Fatalf("snapshot %#v", snapshot)
	}
	if err := k.SetOAuthModelEnabled(t.Context(), "claude-budget", false, "antigravity"); err != nil {
		t.Fatal(err)
	}
	caps, _, err := k.OAuthReasoningCaps(t.Context(), "antigravity")
	if err != nil || caps["claude-budget"] != "" || caps["shared"] != "medium" {
		t.Fatalf("disabled caps=%v err=%v", caps, err)
	}
	if err := k.ApplyOAuthPreset(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	after, err := k.OAuthPresetSnapshot(t.Context())
	if err != nil || !OAuthPresetSnapshotsEqual(snapshot, after) {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	if len(f.aliases["codex"]) != 1 {
		t.Fatal("foreign aliases changed")
	}
}

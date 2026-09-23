package service

import (
	"context"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
	"gopkg.in/yaml.v3"
)

type reasoningCPAMP struct {
	cpamp.API
	config   []byte
	models   []cpamp.OAuthModelDefinition
	excluded []string
	puts     int
}

func (f *reasoningCPAMP) GetConfigYAML(context.Context) ([]byte, error) {
	return append([]byte(nil), f.config...), nil
}

func (f *reasoningCPAMP) PutConfigYAML(_ context.Context, data []byte) error {
	f.config = append([]byte(nil), data...)
	f.puts++
	return nil
}

func (f *reasoningCPAMP) ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error) {
	return f.models, nil
}

func (f *reasoningCPAMP) ListOAuthExcludedModels(context.Context, string) ([]string, error) {
	return f.excluded, nil
}

func TestReasoningCapsPreserveOtherConfigAndClampOnlyExcess(t *testing.T) {
	fake := &reasoningCPAMP{
		config: []byte("# important comment\napi-keys:\n  - secret-value\npayload:\n  override-raw:\n    - models:\n        - name: other-model\n          protocol: codex\n      params:\n        service_tier: '\"priority\"'\n"),
		models: []cpamp.OAuthModelDefinition{{ID: "gpt-6-luna", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}}},
	}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-luna", Cap: "high"}}, revision); err != nil {
		t.Fatal(err)
	}
	if fake.puts != 1 || !strings.Contains(string(fake.config), "secret-value") || !strings.Contains(string(fake.config), "important comment") || !strings.Contains(string(fake.config), "service_tier") {
		t.Fatalf("other CPA settings were not preserved: puts=%d", fake.puts)
	}
	caps, nextRevision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil || caps["gpt-6-luna"] != "high" || nextRevision == revision {
		t.Fatalf("caps=%#v revision changed=%v err=%v", caps, nextRevision != revision, err)
	}
	doc, rules, err := parseReasoningConfig(fake.config)
	if err != nil || doc == nil || rules == nil {
		t.Fatalf("managed YAML invalid: %v", err)
	}
	foundTop, foundUpdate := false, false
	for _, rule := range rules.Content {
		params, _ := yamlField(rule, "params")
		if params == nil || len(params.Content) != 2 {
			t.Fatal("missing managed params")
		}
		path := params.Content[0].Value
		if path == "reasoning.effort" {
			models, _ := yamlField(rule, "models")
			match, _ := yamlField(models.Content[0], "match")
			if match != nil && len(match.Content) == 1 {
				condition, _ := yamlField(match.Content[0], "reasoning.effort")
				foundTop = foundTop || (condition != nil && condition.Value == "xhigh")
			}
		}
		if strings.Contains(path, `type=="configuration_update"&&reasoning.effort=="xhigh"`) {
			foundUpdate = true
		}
	}
	if !foundTop || !foundUpdate {
		t.Fatalf("missing top-level or configuration_update clamp: top=%v update=%v", foundTop, foundUpdate)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-luna", Cap: ""}}, nextRevision); err != nil {
		t.Fatal(err)
	}
	caps, _, err = k.OAuthReasoningCaps(context.Background())
	if err != nil || caps["gpt-6-luna"] != "" || !strings.Contains(string(fake.config), "service_tier") {
		t.Fatalf("clearing cap removed unrelated rules: caps=%#v err=%v", caps, err)
	}
	var parsed yaml.Node
	if err := yaml.Unmarshal(fake.config, &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestReasoningCapsRejectStaleRevisionAndUnsupportedLevel(t *testing.T) {
	fake := &reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{{ID: "gpt-6-luna", ThinkingLevels: []string{"low", "medium", "high"}}},
	}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-luna", Cap: "max"}}, revision); err == nil || fake.puts != 0 {
		t.Fatalf("unsupported level was accepted: puts=%d", fake.puts)
	}
	fake.config = append(fake.config, []byte("# external change\n")...)
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-luna", Cap: "high"}}, revision); err == nil || fake.puts != 0 {
		t.Fatalf("stale form overwrote config: puts=%d", fake.puts)
	}
}

func TestReasoningCapsRejectIncompleteManagedRules(t *testing.T) {
	fake := &reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{{ID: "gpt-6-luna", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}}},
	}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-luna", Cap: "high"}}, revision); err != nil {
		t.Fatal(err)
	}
	doc, rules, err := parseReasoningConfig(fake.config)
	if err != nil {
		t.Fatal(err)
	}
	rules.Content = rules.Content[:len(rules.Content)-1]
	fake.config, err = yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.OAuthReasoningCaps(context.Background()); err == nil {
		t.Fatal("incomplete cap rules were accepted")
	}
}

func TestReasoningCapsRecognizeGroupAfterCPACommentRewrite(t *testing.T) {
	fake := &reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{{ID: "gpt-6-sol", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}}},
	}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-sol", Cap: "medium"}}, revision); err != nil {
		t.Fatal(err)
	}
	doc, rules, err := parseReasoningConfig(fake.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Content) != 8 {
		t.Fatalf("rule count = %d", len(rules.Content))
	}
	for _, rule := range rules.Content {
		models, _ := yamlField(rule, "models")
		name, _ := yamlField(models.Content[0], "name")
		name.LineComment = ""
		rule.HeadComment = "# " + reasoningCapMarker
	}
	fake.config, err = yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	fake.config = normalizeStandaloneComments(fake.config)
	_, rewrittenRules, err := parseReasoningConfig(fake.config)
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, rule := range rewrittenRules.Content {
		if isManagedReasoningRule(rule) {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("expected CPA to detach later standalone comments, got %d marked rules", marked)
	}
	caps, nextRevision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil || caps["gpt-6-sol"] != "medium" {
		t.Fatalf("rewritten comments were not recognized: caps=%v err=%v", caps, err)
	}
	if err := k.SetOAuthReasoningCaps(context.Background(), []ReasoningCapInput{{Model: "gpt-6-sol", Cap: ""}}, nextRevision); err != nil {
		t.Fatal(err)
	}
	_, rules, err = parseReasoningConfig(fake.config)
	if err != nil || rules == nil {
		t.Fatalf("clearing a rewritten group could not be read: err=%v", err)
	}
	if len(rules.Content) != 0 {
		t.Fatalf("clearing a rewritten group left %d rules", len(rules.Content))
	}
}

func TestReasoningCapsInlineMarkersSurviveCPACommentRewrite(t *testing.T) {
	fake := &reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{
			{ID: "gpt-6-luna", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
			{ID: "gpt-6-sol", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		},
	}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inputs := []ReasoningCapInput{{Model: "gpt-6-luna", Cap: "high"}, {Model: "gpt-6-sol", Cap: "medium"}}
	if err := k.SetOAuthReasoningCaps(context.Background(), inputs, revision); err != nil {
		t.Fatal(err)
	}
	fake.config = normalizeStandaloneComments(fake.config)
	caps, _, err := k.OAuthReasoningCaps(context.Background())
	if err != nil || caps["gpt-6-luna"] != "high" || caps["gpt-6-sol"] != "medium" {
		t.Fatalf("inline markers were not preserved: caps=%v err=%v", caps, err)
	}
}

func normalizeStandaloneComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	for index, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			lines[index] = trimmed
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
	"gopkg.in/yaml.v3"
)

type presetCPAMP struct {
	reasoningCPAMP
	aliases       []cpamp.OAuthModelAlias
	failConfigPut bool
}

func (f *presetCPAMP) PutConfigYAML(ctx context.Context, data []byte) error {
	if f.failConfigPut {
		return errors.New("config write failed")
	}
	return f.reasoningCPAMP.PutConfigYAML(ctx, data)
}

func TestOAuthPresetSnapshotsEqual(t *testing.T) {
	left := OAuthPresetSnapshot{
		Channel:       "codex",
		Version:       OAuthPresetVersion,
		Models:        []OAuthPresetModel{{ID: "gpt-b", Enabled: false}, {ID: "GPT-A", Enabled: true}},
		Aliases:       []cpamp.OAuthModelAlias{{Name: "GPT-A", Alias: "client-a", Fork: true}},
		ReasoningCaps: map[string]string{"GPT-A": "HIGH", "gpt-b": ""},
	}
	right := OAuthPresetSnapshot{
		Channel:       "codex",
		Version:       OAuthPresetVersion,
		Models:        []OAuthPresetModel{{ID: "gpt-a", Enabled: true}, {ID: "GPT-B", Enabled: false}},
		Aliases:       []cpamp.OAuthModelAlias{{Name: "gpt-a", Alias: "CLIENT-A", Fork: true}},
		ReasoningCaps: map[string]string{"gpt-a": "high"},
	}
	if !OAuthPresetSnapshotsEqual(left, right) {
		t.Fatal("equivalent snapshots should match")
	}
	right.ReasoningCaps["gpt-a"] = "medium"
	if OAuthPresetSnapshotsEqual(left, right) {
		t.Fatal("different reasoning caps should not match")
	}
}

func (f *presetCPAMP) ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error) {
	return append([]cpamp.OAuthModelAlias(nil), f.aliases...), nil
}

func (f *presetCPAMP) SetOAuthModelAliases(_ context.Context, _ string, aliases []cpamp.OAuthModelAlias) error {
	f.aliases = append([]cpamp.OAuthModelAlias(nil), aliases...)
	return nil
}

func (f *presetCPAMP) SetOAuthExcludedModels(_ context.Context, _ string, excluded []string) error {
	f.excluded = append([]string(nil), excluded...)
	return nil
}

func TestDisablingOAuthModelRemovesAliasesAndManagedReasoningCap(t *testing.T) {
	ctx := context.Background()
	fake := &presetCPAMP{reasoningCPAMP: reasoningCPAMP{
		config: []byte("api-keys: []\npayload:\n  override-raw:\n    - models:\n        - name: other-model\n          protocol: codex\n      params:\n        service_tier: '\"priority\"'\n"),
		models: []cpamp.OAuthModelDefinition{
			{ID: "gpt-alpha", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}},
			{ID: "gpt-beta", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}},
		},
	}, aliases: []cpamp.OAuthModelAlias{{Name: "gpt-alpha", Alias: "old-alpha"}, {Name: "gpt-beta", Alias: "beta-alias"}}}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(ctx, []ReasoningCapInput{{Model: "gpt-alpha", Cap: "medium"}, {Model: "gpt-beta", Cap: "high"}}, revision); err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthModelEnabled(ctx, "gpt-alpha", false); err != nil {
		t.Fatal(err)
	}
	if len(fake.excluded) != 1 || fake.excluded[0] != "gpt-alpha" || len(fake.aliases) != 1 || fake.aliases[0].Alias != "beta-alias" {
		t.Fatalf("disabled model retained aliases or changed another model: excluded=%v aliases=%v", fake.excluded, fake.aliases)
	}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil || caps["gpt-alpha"] != "" || caps["gpt-beta"] != "high" || !strings.Contains(string(fake.config), "service_tier") {
		t.Fatalf("disabled model cap remained or unrelated rules changed: caps=%v err=%v", caps, err)
	}
	if err := k.SetOAuthModelEnabled(ctx, "gpt-alpha", true); err != nil {
		t.Fatal(err)
	}
	caps, _, err = k.OAuthReasoningCaps(ctx)
	if err != nil || caps["gpt-alpha"] != "" || caps["gpt-beta"] != "high" {
		t.Fatalf("re-enabled model unexpectedly restored its old cap: caps=%v err=%v", caps, err)
	}
}

func TestDisablingOAuthModelRollsBackIfReasoningCapCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	fake := &presetCPAMP{reasoningCPAMP: reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{{ID: "gpt-alpha", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}}},
	}, aliases: []cpamp.OAuthModelAlias{{Name: "gpt-alpha", Alias: "old-alpha"}}}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(ctx, []ReasoningCapInput{{Model: "gpt-alpha", Cap: "medium"}}, revision); err != nil {
		t.Fatal(err)
	}
	fake.failConfigPut = true
	if err := k.SetOAuthModelEnabled(ctx, "gpt-alpha", false); err == nil {
		t.Fatal("model disable succeeded after reasoning rule cleanup failed")
	}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil || caps["gpt-alpha"] != "medium" || len(fake.excluded) != 0 || len(fake.aliases) != 1 {
		t.Fatalf("failed disable was not rolled back: caps=%v excluded=%v aliases=%v err=%v", caps, fake.excluded, fake.aliases, err)
	}
}

func TestDisablingAlreadyExcludedModelsRemovesNormalizedDuplicateCaps(t *testing.T) {
	ctx := context.Background()
	fake := &presetCPAMP{reasoningCPAMP: reasoningCPAMP{
		config: []byte("api-keys: []\npayload:\n  override-raw:\n    - models:\n        - name: unrelated\n          protocol: codex\n      params:\n        service_tier: '\"priority\"'\n"),
		models: []cpamp.OAuthModelDefinition{
			{ID: "gpt-6-sol", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
			{ID: "gpt-6.1-sol", ThinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		},
	}}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []ReasoningCapInput{{Model: "gpt-6-sol", Cap: "medium"}, {Model: "gpt-6.1-sol", Cap: "medium"}}
	if err := k.SetOAuthReasoningCaps(ctx, inputs, revision); err != nil {
		t.Fatal(err)
	}
	doc, rules, err := parseReasoningConfig(fake.config)
	if err != nil {
		t.Fatal(err)
	}
	generated := append([]*yaml.Node(nil), rules.Content[1:]...)
	for _, rule := range generated {
		models, _ := yamlField(rule, "models")
		model := models.Content[0]
		name, _ := yamlField(model, "name")
		name.LineComment = ""
		rule.HeadComment = ""
		model.Content = append(model.Content,
			yamlScalar("headers"), yamlMap(),
			yamlScalar("from-protocol"), yamlScalar(""),
			yamlScalar("not-match"), yamlSeq(),
			yamlScalar("exist"), yamlSeq(),
			yamlScalar("not-exist"), yamlSeq(),
		)
		if match, _ := yamlField(model, "match"); match == nil {
			model.Content = append(model.Content, yamlScalar("match"), yamlSeq())
		}
	}
	rules.Content = append(rules.Content, generated...)
	fake.config, err = yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	fake.excluded = []string{"gpt-6-sol", "gpt-6.1-sol"}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil || caps["gpt-6-sol"] != "medium" || caps["gpt-6.1-sol"] != "medium" {
		t.Fatalf("normalized duplicate caps not recognized: caps=%v err=%v", caps, err)
	}
	copyFake := &reasoningCPAMP{config: append([]byte(nil), fake.config...), models: fake.models}
	copyKeys := &Keys{CPAMP: copyFake}
	_, copyRevision, err := copyKeys.OAuthReasoningCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyKeys.SetOAuthReasoningCaps(ctx, inputs, copyRevision); err != nil {
		t.Fatal(err)
	}
	_, deduplicated, err := parseReasoningConfig(copyFake.config)
	if err != nil || len(deduplicated.Content) != 17 {
		t.Fatalf("saving the same caps did not deduplicate legacy groups: rules=%v err=%v", deduplicated, err)
	}
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		if err := k.SetOAuthModelEnabled(ctx, model, false); err != nil {
			t.Fatalf("remove stale cap for %s: %v", model, err)
		}
	}
	caps, _, err = k.OAuthReasoningCaps(ctx)
	if err != nil || len(caps) != 0 || !strings.Contains(string(fake.config), "service_tier") {
		t.Fatalf("stale caps remained or unrelated rule changed: caps=%v err=%v", caps, err)
	}
}

func TestOAuthPresetSnapshotAndApplyRestoresAllManagedAreas(t *testing.T) {
	ctx := context.Background()
	fake := &presetCPAMP{reasoningCPAMP: reasoningCPAMP{
		config: []byte("api-keys: []\n"),
		models: []cpamp.OAuthModelDefinition{
			{ID: "gpt-alpha", ThinkingLevels: []string{"low", "medium", "high", "xhigh"}},
			{ID: "gpt-beta", ThinkingLevels: []string{"low", "medium", "high"}},
		},
		excluded: []string{"gpt-beta"},
	}, aliases: []cpamp.OAuthModelAlias{{Name: "gpt-alpha", Alias: "daily", Fork: true, DisplayName: "daily", ForceMapping: true}}}
	k := &Keys{CPAMP: fake}
	_, revision, err := k.OAuthReasoningCaps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetOAuthReasoningCaps(ctx, []ReasoningCapInput{{Model: "gpt-alpha", Cap: "high"}}, revision); err != nil {
		t.Fatal(err)
	}
	snapshot, err := k.OAuthPresetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 2 || len(snapshot.Aliases) != 1 || snapshot.ReasoningCaps["gpt-alpha"] != "high" {
		t.Fatalf("snapshot=%#v", snapshot)
	}

	fake.excluded = []string{"gpt-alpha"}
	fake.aliases = []cpamp.OAuthModelAlias{{Name: "gpt-beta", Alias: "temporary", ForceMapping: true}}
	fake.config = []byte("api-keys: []\n")
	if err := k.ApplyOAuthPreset(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	models, _, err := k.OAuthModelSettings(ctx)
	if err != nil || !models[0].Enabled || models[1].Enabled {
		t.Fatalf("models=%#v err=%v", models, err)
	}
	if len(fake.aliases) != 1 || fake.aliases[0].Name != "gpt-alpha" || fake.aliases[0].Alias != "daily" || !fake.aliases[0].Fork {
		t.Fatalf("aliases=%#v", fake.aliases)
	}
	caps, _, err := k.OAuthReasoningCaps(ctx)
	if err != nil || caps["gpt-alpha"] != "high" {
		t.Fatalf("caps=%#v err=%v", caps, err)
	}
}

func TestOAuthPresetRejectsChangedCatalogBeforeMutation(t *testing.T) {
	ctx := context.Background()
	fake := &presetCPAMP{reasoningCPAMP: reasoningCPAMP{config: []byte("api-keys: []\n"), models: []cpamp.OAuthModelDefinition{{ID: "gpt-alpha"}}}}
	k := &Keys{CPAMP: fake}
	snapshot, err := k.OAuthPresetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fake.models = append(fake.models, cpamp.OAuthModelDefinition{ID: "gpt-new"})
	if err := k.ApplyOAuthPreset(ctx, snapshot); err == nil {
		t.Fatal("changed model catalog was accepted")
	}
	if len(fake.aliases) != 0 || len(fake.excluded) != 0 {
		t.Fatalf("configuration mutated: aliases=%#v excluded=%#v", fake.aliases, fake.excluded)
	}
}

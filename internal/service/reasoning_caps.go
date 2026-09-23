package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"cliproxy-portal/internal/security"
	"gopkg.in/yaml.v3"
)

const reasoningCapMarker = "cliproxy-portal:reasoning-cap:v1"

var reasoningEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// ReasoningCapInput sets the highest permitted effort for one enabled Codex
// model. An empty Cap removes the portal-managed limit for that model.
type ReasoningCapInput struct {
	Model string
	Cap   string
}

func reasoningEffortIndex(level string) int {
	for i, candidate := range reasoningEffortOrder {
		if candidate == level {
			return i
		}
	}
	return -1
}

// OAuthReasoningCaps reads only portal-owned limits. The full CPA YAML, which
// may contain credentials, never leaves the service layer.
func (k *Keys) OAuthReasoningCaps(ctx context.Context) (map[string]string, string, error) {
	client, ok := k.CPAMP.(interface {
		GetConfigYAML(context.Context) ([]byte, error)
	})
	if !ok {
		return nil, "", errors.New("CPAMP 客户端不支持 CPA 配置管理")
	}
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return nil, "", err
	}
	_, rules, err := parseReasoningConfig(data)
	if err != nil {
		return nil, "", err
	}
	caps, err := managedReasoningCaps(rules)
	if err != nil {
		return nil, "", err
	}
	return caps, security.SHA256(string(data)), nil
}

// SetOAuthReasoningCaps preserves every non-portal CPA rule and all other YAML
// fields. Portal-managed rules are appended last within override-raw, so they
// win over ordinary overrides without replacing lower requested efforts.
func (k *Keys) SetOAuthReasoningCaps(ctx context.Context, inputs []ReasoningCapInput, revision string) error {
	k.modelConfigMu.Lock()
	defer k.modelConfigMu.Unlock()
	client, ok := k.CPAMP.(interface {
		GetConfigYAML(context.Context) ([]byte, error)
		PutConfigYAML(context.Context, []byte) error
	})
	if !ok {
		return errors.New("CPAMP 客户端不支持 CPA 配置管理")
	}
	models, _, err := k.OAuthModelSettings(ctx)
	if err != nil {
		return err
	}
	editable := make(map[string]OAuthModelSetting)
	for _, model := range models {
		if model.Enabled && len(model.ThinkingLevels) > 0 {
			editable[strings.ToLower(model.ID)] = model
		}
	}
	if len(inputs) != len(editable) {
		return errors.New("模型列表已变化，请刷新页面后重试")
	}
	selected := make(map[string]string, len(inputs))
	canonical := make(map[string]string, len(inputs))
	for _, input := range inputs {
		modelID := strings.ToLower(strings.TrimSpace(input.Model))
		model, exists := editable[modelID]
		if !exists {
			return errors.New("模型列表包含未知或重复项，请刷新页面后重试")
		}
		if _, duplicate := selected[modelID]; duplicate {
			return errors.New("模型列表包含未知或重复项，请刷新页面后重试")
		}
		cap := strings.ToLower(strings.TrimSpace(input.Cap))
		if cap != "" && (cap == "ultra" || reasoningEffortIndex(cap) < 0 || !containsReasoningLevel(model.ThinkingLevels, cap)) {
			return fmt.Errorf("模型 %s 不支持所选思考强度", model.ID)
		}
		selected[modelID] = cap
		canonical[modelID] = model.ID
	}
	data, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	if revision != security.SHA256(string(data)) {
		return errors.New("CPA 配置已由其他操作修改，请刷新页面后重试")
	}
	doc, rules, err := parseReasoningConfig(data)
	if err != nil {
		return err
	}
	current, err := managedReasoningCaps(rules)
	if err != nil {
		return err
	}
	changed := false
	for modelID, cap := range selected {
		if current[modelID] != cap {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	if err := ensureReasoningRuleSlot(doc); err != nil {
		return err
	}
	_, rules, err = reasoningOverrideRaw(doc)
	if err != nil {
		return err
	}
	_, managedIndexes, err := parseManagedReasoningGroups(rules)
	if err != nil {
		return err
	}
	kept := make([]*yaml.Node, 0, len(rules.Content))
	for index, rule := range rules.Content {
		if modelID, managed := managedIndexes[index]; managed {
			if _, replacing := selected[modelID]; replacing {
				continue
			}
		}
		kept = append(kept, rule)
	}
	rules.Content = kept
	ids := make([]string, 0, len(selected))
	for id, cap := range selected {
		if cap != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		rules.Content = append(rules.Content, buildReasoningCapRules(canonical[id], selected[id])...)
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return errors.New("CPA 配置编码失败")
	}
	if err := encoder.Close(); err != nil {
		return errors.New("CPA 配置编码失败")
	}
	latest, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, data) {
		return errors.New("CPA 配置在保存期间发生变化，请刷新页面后重试")
	}
	if err := client.PutConfigYAML(ctx, output.Bytes()); err != nil {
		return err
	}
	confirmed, err := client.GetConfigYAML(ctx)
	if err != nil {
		return err
	}
	_, confirmedRules, err := parseReasoningConfig(confirmed)
	if err != nil {
		return err
	}
	confirmedCaps, err := managedReasoningCaps(confirmedRules)
	if err != nil {
		return err
	}
	for id, cap := range selected {
		if confirmedCaps[id] != cap {
			return errors.New("CPA 未确认思考强度上限变更")
		}
	}
	return nil
}

func containsReasoningLevel(levels []string, candidate string) bool {
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), candidate) {
			return true
		}
	}
	return false
}

func parseReasoningConfig(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("CPA 配置无法解析，未作修改")
	}
	_, rules, err := reasoningOverrideRaw(&doc)
	return &doc, rules, err
}

func reasoningOverrideRaw(doc *yaml.Node) (*yaml.Node, *yaml.Node, error) {
	root := doc.Content[0]
	payload, err := yamlField(root, "payload")
	if err != nil || payload == nil {
		return nil, nil, err
	}
	if payload.Kind != yaml.MappingNode {
		return nil, nil, errors.New("CPA payload 配置结构不受支持，未作修改")
	}
	rules, err := yamlField(payload, "override-raw")
	if err != nil || rules == nil {
		return payload, nil, err
	}
	if rules.Kind != yaml.SequenceNode {
		return nil, nil, errors.New("CPA override-raw 配置结构不受支持，未作修改")
	}
	return payload, rules, nil
}

func ensureReasoningRuleSlot(doc *yaml.Node) error {
	root := doc.Content[0]
	payload, err := yamlField(root, "payload")
	if err != nil {
		return err
	}
	if payload == nil {
		payload = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, yamlScalar("payload"), payload)
	}
	if payload.Kind != yaml.MappingNode {
		return errors.New("CPA payload 配置结构不受支持，未作修改")
	}
	rules, err := yamlField(payload, "override-raw")
	if err != nil {
		return err
	}
	if rules == nil {
		payload.Content = append(payload.Content, yamlScalar("override-raw"), &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
	}
	return nil
}

func yamlField(node *yaml.Node, key string) (*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, errors.New("CPA 配置结构不受支持，未作修改")
	}
	var found *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		if found != nil {
			return nil, errors.New("CPA 配置含有重复字段，未作修改")
		}
		found = node.Content[i+1]
	}
	return found, nil
}

func managedReasoningCaps(rules *yaml.Node) (map[string]string, error) {
	caps, _, err := parseManagedReasoningGroups(rules)
	return caps, err
}

// CPA left-aligns standalone YAML comments after other management operations.
// A marker on the first rule identifies the entire contiguous, structurally
// validated group, even if later legacy markers stop attaching to rule nodes.
func parseManagedReasoningGroups(rules *yaml.Node) (map[string]string, map[int]string, error) {
	caps := make(map[string]string)
	managedIndexes := make(map[int]string)
	if rules == nil {
		return caps, managedIndexes, nil
	}
	for index := 0; index < len(rules.Content); {
		rule := rules.Content[index]
		if !isManagedReasoningRule(rule) {
			index++
			continue
		}
		model, cap, err := managedReasoningRule(rule)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := caps[model]; duplicate {
			return nil, nil, errors.New("CPA 思考强度规则相互冲突，未作修改")
		}
		expected := buildReasoningCapRules(model, cap)
		if len(expected) == 0 || index+len(expected) > len(rules.Content) {
			return nil, nil, errors.New("CPA 中的门户思考强度规则不完整，未作修改")
		}
		for offset, want := range expected {
			if !sameReasoningRule(rules.Content[index+offset], want) {
				return nil, nil, errors.New("CPA 中的门户思考强度规则已被修改，未作修改")
			}
			managedIndexes[index+offset] = model
		}
		caps[model] = cap
		index += len(expected)
	}
	return caps, managedIndexes, nil
}

func sameReasoningRule(left, right *yaml.Node) bool {
	var leftValue, rightValue any
	return left.Decode(&leftValue) == nil && right.Decode(&rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

func isManagedReasoningRule(rule *yaml.Node) bool {
	if rule == nil {
		return false
	}
	if strings.Contains(rule.HeadComment, reasoningCapMarker) {
		return true
	}
	models, err := yamlField(rule, "models")
	if err != nil || models == nil || models.Kind != yaml.SequenceNode || len(models.Content) == 0 {
		return false
	}
	name, err := yamlField(models.Content[0], "name")
	return err == nil && name != nil && strings.Contains(name.LineComment, reasoningCapMarker)
}

func managedReasoningRule(rule *yaml.Node) (string, string, error) {
	invalid := errors.New("CPA 中的门户思考强度规则无效，未作修改")
	if rule == nil || rule.Kind != yaml.MappingNode {
		return "", "", invalid
	}
	models, err := yamlField(rule, "models")
	if err != nil || models == nil || models.Kind != yaml.SequenceNode || len(models.Content) != 1 {
		return "", "", invalid
	}
	name, err := yamlField(models.Content[0], "name")
	if err != nil || name == nil || strings.TrimSpace(name.Value) == "" {
		return "", "", invalid
	}
	params, err := yamlField(rule, "params")
	if err != nil || params == nil || params.Kind != yaml.MappingNode || len(params.Content) != 2 {
		return "", "", invalid
	}
	var cap string
	if json.Unmarshal([]byte(params.Content[1].Value), &cap) != nil || reasoningEffortIndex(cap) < 0 {
		return "", "", invalid
	}
	return strings.ToLower(strings.TrimSpace(name.Value)), cap, nil
}

func buildReasoningCapRules(model, cap string) []*yaml.Node {
	capIndex := reasoningEffortIndex(cap)
	result := make([]*yaml.Node, 0, (len(reasoningEffortOrder)-capIndex-1)*2)
	for _, excessive := range reasoningEffortOrder[capIndex+1:] {
		result = append(result, buildReasoningRule(model, cap, excessive, false))
		result = append(result, buildReasoningRule(model, cap, excessive, true))
	}
	if len(result) > 0 {
		models, _ := yamlField(result[0], "models")
		name, _ := yamlField(models.Content[0], "name")
		name.LineComment = "# " + reasoningCapMarker
	}
	return result
}

func buildReasoningRule(model, cap, excessive string, update bool) *yaml.Node {
	selector := yamlMap(
		"name", yamlScalar(model),
		"protocol", yamlScalar("codex"),
	)
	path := "reasoning.effort"
	if update {
		path = `input.#(type=="configuration_update"&&reasoning.effort=="` + excessive + `")#.reasoning.effort`
	} else {
		selector.Content = append(selector.Content, yamlScalar("match"), yamlSeq(yamlMap("reasoning.effort", yamlScalar(excessive))))
	}
	rawValue, _ := json.Marshal(cap)
	return yamlMap("models", yamlSeq(selector), "params", yamlMap(path, yamlScalar(string(rawValue))))
}

func yamlScalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func yamlMap(entries ...any) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(entries); i += 2 {
		node.Content = append(node.Content, yamlScalar(entries[i].(string)), entries[i+1].(*yaml.Node))
	}
	return node
}

func yamlSeq(values ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: values}
}

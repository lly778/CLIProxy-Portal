package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const antigravityReasoningMarker = "cliproxy-portal:antigravity-reasoning-cap:v1"
const antigravityThinkingPath = "generationConfig.thinkingConfig."

// CPA's standard discrete-level to token-budget conversion. The selected
// model's maximum is applied separately, including Claude's 64K ceiling.
var antigravityEffortBudgets = map[string]int{"none": 0, "minimal": 512, "low": 1024, "medium": 8192, "high": 24576, "xhigh": 32768, "max": 128000}

func validateReasoningChannel(channel string) error {
	if channel != "codex" && channel != "antigravity" {
		return errors.New("该渠道暂不支持思考强度上限")
	}
	return nil
}

// OAuthReasoningLevels exposes actual declared levels, or the supported
// equivalent budgets for budget-only Antigravity models. Auto is deliberately
// not an ordered level; absent/default/automatic requests are not upgraded.
func OAuthReasoningLevels(channel string, model OAuthModelSetting) []string {
	if channel != "antigravity" {
		return model.ThinkingLevels
	}
	levels := []string{}
	for _, level := range reasoningEffortOrder {
		budget, ok := antigravityEffortBudgets[level]
		if !ok {
			continue
		}
		if len(model.ThinkingLevels) > 0 {
			if containsReasoningLevel(model.ThinkingLevels, level) {
				levels = append(levels, level)
			}
			continue
		}
		if model.ThinkingMax <= 0 {
			continue
		}
		if level == "none" {
			if model.ThinkingZeroAllowed {
				levels = append(levels, level)
			}
			continue
		}
		if budget >= model.ThinkingMin {
			levels = append(levels, level)
		}
	}
	return levels
}

func OAuthReasoningBudget(channel string, model OAuthModelSetting, cap string) (int, bool) {
	if channel != "antigravity" || model.ThinkingMax <= 0 {
		return 0, false
	}
	budget, ok := antigravityEffortBudgets[cap]
	if !ok {
		return 0, false
	}
	if budget > model.ThinkingMax {
		budget = model.ThinkingMax
	}
	return budget, true
}

func buildOAuthReasoningCapRules(channel string, model OAuthModelSetting, cap string) []*yaml.Node {
	if channel == "codex" {
		return buildReasoningCapRules(model.ID, cap)
	}
	budget := antigravityEffortBudgets[cap]
	if model.ThinkingMax > 0 && budget > model.ThinkingMax {
		budget = model.ThinkingMax
	}
	return buildAntigravityReasoningRules(model.ID, cap, budget)
}

func buildAntigravityReasoningRules(model, cap string, budget int) []*yaml.Node {
	index := reasoningEffortIndex(cap)
	if index < 0 || cap == "ultra" {
		return nil
	}
	levelPath := antigravityThinkingPath + "thinkingLevel"
	raw, _ := json.Marshal(cap)
	rules := []*yaml.Node{}
	for _, excessive := range reasoningEffortOrder[index+1:] {
		selector := yamlMap("name", yamlScalar(model), "protocol", yamlScalar("antigravity"), "match", yamlSeq(yamlMap(levelPath, yamlScalar(excessive))))
		rules = append(rules, yamlMap("models", yamlSeq(selector), "params", yamlMap(levelPath, yamlScalar(string(raw)))))
	}
	// CPA treats paths as relative to its Antigravity "request" envelope.
	// A GJSON singleton projection makes scalar numeric comparison possible in
	// its existing array-query condition resolver; the destination remains the
	// ordinary budget field. Lower/equal/-1/absent budgets do not match.
	guard := antigravityThinkingPath + "@this|[thinkingBudget].#(>" + strconv.Itoa(budget) + ")"
	selector := yamlMap("name", yamlScalar(model), "protocol", yamlScalar("antigravity"), "exist", yamlSeq(yamlScalar(guard)))
	rules = append(rules, yamlMap("models", yamlSeq(selector), "params", yamlMap(antigravityThinkingPath+"thinkingBudget", yamlScalar(strconv.Itoa(budget)))))
	models, _ := yamlField(rules[0], "models")
	name, _ := yamlField(models.Content[0], "name")
	name.LineComment = "# " + antigravityReasoningMarker
	return rules
}

func managedReasoningCapsForChannel(channel string, rules *yaml.Node) (map[string]string, error) {
	caps, _, err := parseManagedReasoningGroupsForChannel(channel, rules)
	return caps, err
}

func parseManagedReasoningGroupsForChannel(channel string, rules *yaml.Node) (map[string]string, map[int]string, error) {
	// Validate both channels' owned groups before any configuration write.
	codex, codexIndexes, err := parseManagedReasoningGroups(rules)
	if err != nil {
		return nil, nil, err
	}
	anti, antiIndexes, err := parseAntigravityReasoningGroups(rules)
	if err != nil {
		return nil, nil, err
	}
	if channel == "codex" {
		return codex, codexIndexes, nil
	}
	return anti, antiIndexes, nil
}

func parseAntigravityReasoningGroups(rules *yaml.Node) (map[string]string, map[int]string, error) {
	caps := map[string]string{}
	indexes := map[int]string{}
	if rules == nil {
		return caps, indexes, nil
	}
	invalid := errors.New("CPA 中的门户 Antigravity 思考强度规则已被修改，未作修改")
	for i := 0; i < len(rules.Content); {
		rule := rules.Content[i]
		models, _ := yamlField(rule, "models")
		var selector *yaml.Node
		if models != nil && models.Kind == yaml.SequenceNode && len(models.Content) == 1 {
			selector = models.Content[0]
		}
		name, _ := yamlField(selector, "name")
		protocol, _ := yamlField(selector, "protocol")
		marked := strings.Contains(rule.HeadComment, antigravityReasoningMarker) || (name != nil && strings.Contains(name.LineComment, antigravityReasoningMarker))
		if name == nil || protocol == nil || protocol.Value != "antigravity" {
			if marked {
				return nil, nil, invalid
			}
			i++
			continue
		}
		params, _ := yamlField(rule, "params")
		if params == nil || params.Kind != yaml.MappingNode || len(params.Content) != 2 || params.Content[0].Value != antigravityThinkingPath+"thinkingLevel" {
			if marked {
				return nil, nil, invalid
			}
			i++
			continue
		}
		var cap string
		if json.Unmarshal([]byte(params.Content[1].Value), &cap) != nil || cap == "ultra" || reasoningEffortIndex(cap) < 0 {
			if marked {
				return nil, nil, invalid
			}
			i++
			continue
		}
		levelCount := len(reasoningEffortOrder) - reasoningEffortIndex(cap) - 1
		end := i + levelCount
		matched := end < len(rules.Content)
		budget := 0
		if matched {
			lastParams, _ := yamlField(rules.Content[end], "params")
			if lastParams == nil || lastParams.Kind != yaml.MappingNode || len(lastParams.Content) != 2 || lastParams.Content[0].Value != antigravityThinkingPath+"thinkingBudget" {
				matched = false
			} else {
				var err error
				budget, err = strconv.Atoi(lastParams.Content[1].Value)
				if err != nil || budget < 0 || budget > antigravityEffortBudgets[cap] || (cap != "none" && budget == 0) {
					matched = false
				}
			}
		}
		if matched {
			expected := buildAntigravityReasoningRules(name.Value, cap, budget)
			for offset, want := range expected {
				if !sameReasoningRule(rules.Content[i+offset], want) {
					matched = false
					break
				}
			}
		}
		if !matched {
			if marked {
				return nil, nil, invalid
			}
			i++
			continue
		}
		id := strings.ToLower(strings.TrimSpace(name.Value))
		if previous, exists := caps[id]; exists && previous != cap {
			return nil, nil, invalid
		}
		caps[id] = cap
		for j := i; j <= end; j++ {
			indexes[j] = id
		}
		i = end + 1
	}
	return caps, indexes, nil
}

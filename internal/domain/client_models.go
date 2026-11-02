package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ClientModels is a client preference publication, not a gateway access policy.
// It contains no credentials and is included in the normal database backup.
type ClientModels struct {
	Version      int           `json:"version"`
	Revision     int64         `json:"revision"`
	Enabled      bool          `json:"enabled"`
	DefaultModel string        `json:"default_model"`
	APIBaseURL   string        `json:"api_base_url,omitempty"`
	Models       []ClientModel `json:"models"`
}

type ClientModel struct {
	ID string `json:"id"`
	// Legacy capability overrides are read for compatibility, not published in v2.
	ReasoningLevels  []string `json:"reasoning_levels,omitempty"`
	DefaultReasoning string   `json:"default_reasoning"`
	// Derived at read time; not an administrator-selected capability override.
	SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
}

var clientModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

const ClientModelsVersion = 2

// Defaults are preferences; capabilities remain subject to the model and client.
func ClientReasoningLevels() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

func ValidClientModelID(id string) bool { return clientModelID.MatchString(id) }

func (c ClientModels) Validate() error {
	if (c.Version != 1 && c.Version != ClientModelsVersion) || c.Revision < 0 || len(c.Models) > 128 {
		return errors.New("客户端模型配置版本或数量无效（最多 128 个模型）")
	}
	levels := make(map[string]bool)
	for _, level := range ClientReasoningLevels() {
		levels[level] = true
	}
	seen := make(map[string]bool)
	for _, model := range c.Models {
		if !ValidClientModelID(model.ID) || seen[strings.ToLower(model.ID)] {
			return errors.New("模型名称无效或重复")
		}
		seen[strings.ToLower(model.ID)] = true
		allowed := make(map[string]bool)
		for _, level := range model.ReasoningLevels {
			if !levels[level] || allowed[level] {
				return fmt.Errorf("模型 %s 的思考档位无效或重复", model.ID)
			}
			allowed[level] = true
		}
		if model.DefaultReasoning != "" && !levels[model.DefaultReasoning] {
			return fmt.Errorf("模型 %s 的默认思考强度无效", model.ID)
		}
		if c.Version == 1 && ((len(allowed) == 0 && model.DefaultReasoning != "") || (len(allowed) > 0 && !allowed[model.DefaultReasoning])) {
			return fmt.Errorf("模型 %s 的默认思考强度必须属于勾选档位；不发送思考参数时请选择不指定", model.ID)
		}
	}
	defaultFound := c.DefaultModel == ""
	for _, model := range c.Models {
		defaultFound = defaultFound || model.ID == c.DefaultModel
	}
	if (c.Enabled && len(c.Models) == 0) || !defaultFound || (c.Enabled && c.DefaultModel == "") {
		return errors.New("发布时至少选择一个模型，并从已选模型中指定默认模型")
	}
	return nil
}

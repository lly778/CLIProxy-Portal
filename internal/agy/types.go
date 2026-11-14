// Package agy bridges the official Antigravity ACP agent to client-owned tools
// and official Google web/image services. Model-requested commands and local
// project file operations remain on the client.
package agy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const Prefix = "agy/"
const MaxBody = 10 << 20
const MaxText = 160 << 10

type Model struct {
	ID     string   `json:"id"`
	Name   string   `json:"name,omitempty"`
	Levels []string `json:"levels,omitempty"`
}

type ModelRule struct {
	ID           string   `json:"id"`
	Enabled      bool     `json:"enabled"`
	Alias        string   `json:"alias,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	HideOriginal bool     `json:"hide_original,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Cap          string   `json:"cap,omitempty"`
}

// Settings contains no OAuth credentials or caller keys. Model capabilities are
// obtained from the runtime, never inferred from a model's name.
type Settings struct {
	Revision  int64       `json:"revision"`
	Enabled   bool        `json:"enabled"`
	AdminOnly bool        `json:"admin_only"`
	Models    []Model     `json:"models"`
	Rules     []ModelRule `json:"rules"`
}

func (s Settings) Validate() error {
	if s.Revision < 0 || len(s.Models) > 256 || len(s.Rules) > 256 {
		return errors.New("AGY 配置大小或版本无效")
	}
	known := map[string]Model{}
	for _, m := range s.Models {
		if !validID(m.ID) || len(m.Levels) > 16 {
			return errors.New("AGY 模型声明无效")
		}
		if _, ok := known[m.ID]; ok {
			return errors.New("AGY 模型重复")
		}
		known[m.ID] = m
	}
	seen := map[string]bool{}
	for _, r := range s.Rules {
		m, ok := known[r.ID]
		if !ok || seen[r.ID] || (r.Alias != "" && !validID(r.Alias)) {
			return errors.New("AGY 模型配置无效或重复")
		}
		seen[r.ID] = true
		aliases := r.AllAliases()
		if len(aliases) > 32 || r.HideOriginal && len(aliases) == 0 {
			return errors.New("AGY 别名配置无效")
		}
		for _, alias := range aliases {
			if !validID(alias) {
				return errors.New("AGY 别名无效")
			}
		}
		for _, effort := range []string{r.Effort, r.Cap} {
			if effort != "" {
				found := false
				for _, v := range m.Levels {
					found = found || v == effort
				}
				if !found {
					return errors.New("思考强度不在上游声明中")
				}
			}
		}
	}
	public := map[string]bool{}
	for _, r := range s.Rules {
		for _, id := range append([]string{r.ID}, r.AllAliases()...) {
			if id == "" {
				continue
			}
			id = strings.ToLower(id)
			if public[id] {
				return errors.New("AGY 模型原名或别名冲突")
			}
			public[id] = true
		}
	}
	return nil
}

func (r ModelRule) AllAliases() []string {
	v := append([]string(nil), r.Aliases...)
	if r.Alias != "" {
		v = append(v, r.Alias)
	}
	return v
}

func PublicEffort(v string) string {
	v = strings.ToLower(v)
	if v == "extra" {
		return "xhigh"
	}
	return v
}
func (s Settings) Visible() []Model {
	out := []Model{}
	if !s.Enabled {
		return out
	}
	for _, r := range s.Rules {
		if !r.Enabled {
			continue
		}
		for _, m := range s.Models {
			if m.ID == r.ID {
				m.ID = Prefix + r.ID
				if !r.HideOriginal {
					out = append(out, m)
				}
				for _, alias := range r.AllAliases() {
					m.ID = Prefix + alias
					out = append(out, m)
				}
				break
			}
		}
	}
	return out
}

func validID(v string) bool {
	if v == "" || len(v) > 120 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-:/", c)) {
			return false
		}
	}
	return true
}

func (s Settings) Resolve(public string) (ModelRule, bool) {
	if !s.Enabled || !strings.HasPrefix(public, Prefix) {
		return ModelRule{}, false
	}
	name := strings.TrimPrefix(public, Prefix)
	for _, r := range s.Rules {
		match := name == r.ID && !r.HideOriginal
		for _, alias := range r.AllAliases() {
			match = match || name == alias
		}
		if r.Enabled && match {
			return r, true
		}
	}
	return ModelRule{}, false
}

type Status struct {
	Ready           bool             `json:"ready"`
	State           string           `json:"state"`
	Sessions        int              `json:"sessions"`
	Message         string           `json:"message,omitempty"`
	MaxSessions     int              `json:"max_sessions,omitempty"`
	DiskSessions    int64            `json:"disk_sessions,omitempty"`
	ActiveSessions  int              `json:"active_sessions,omitempty"`
	WaitingSessions int              `json:"waiting_sessions,omitempty"`
	CacheError      bool             `json:"cache_error,omitempty"`
	Runtime         *RuntimeEvidence `json:"runtime,omitempty"`
}

type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string                    { return e.Message }
func fail(status int, code, message string) error { return &Error{status, code, message} }

type Usage struct {
	Input     int64 `json:"input_tokens"`
	Output    int64 `json:"output_tokens"`
	Cache     int64 `json:"cached_tokens"`
	Reasoning int64 `json:"reasoning_tokens"`
	Total     int64 `json:"total_tokens"`
	Known     bool  `json:"known"`
}

func newID(prefix string) string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("secure random unavailable")
	}
	return prefix + hex.EncodeToString(b[:])
}

// Keep persisted HTTP replay identities unchanged across numeric decoder upgrades.
func wireDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	if exact, err := canonicalJSONNumbers(b); err == nil {
		b = exact
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type configOption struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Current  string `json:"currentValue"`
	Options  []struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"options"`
}
type newSession struct {
	ID      string         `json:"sessionId"`
	Options []configOption `json:"configOptions"`
	Models  struct {
		Available []struct {
			ID   string `json:"modelId"`
			Name string `json:"name"`
		} `json:"availableModels"`
	} `json:"models"`
}

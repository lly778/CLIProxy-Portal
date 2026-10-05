package service

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/security"
)

var oauthChannelPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// NormalizeOAuthChannel preserves CPA channel keys; legacy forms default to Codex.
func NormalizeOAuthChannel(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "codex", nil
	}
	if !oauthChannelPattern.MatchString(value) {
		return "", errors.New("OAuth 渠道无效")
	}
	return value, nil
}

func selectedOAuthChannel(channels []string) (string, error) {
	if len(channels) > 1 {
		return "", errors.New("只能选择一个 OAuth 渠道")
	}
	if len(channels) == 0 {
		return "codex", nil
	}
	return NormalizeOAuthChannel(channels[0])
}

func OAuthChannelLabel(channel string) string {
	switch channel {
	case "", "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	case "claude":
		return "Claude"
	case "gemini":
		return "Gemini"
	case "kimi":
		return "Kimi"
	case "xai":
		return "xAI"
	case "devin":
		return "Devin"
	case "meta", "muse":
		return "Muse (Meta)"
	case "gemini-cli":
		return "Gemini CLI"
	case "qwen":
		return "Qwen"
	case "iflow":
		return "iFlow"
	default:
		return channel
	}
}

// OAuthChannels discovers installed credential providers and configured channels.
// Do not expose credentials or list providers not present on this CPA instance.
func (k *Keys) OAuthChannels(ctx context.Context) ([]string, error) {
	return k.oauthChannels(ctx, false)
}

// Mutating routing settings must not silently omit configured channels when
// discovery fails. The read-only channel picker retains its fallback behavior.
func (k *Keys) oauthChannels(ctx context.Context, strict bool) ([]string, error) {
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	var files []cpamp.AuthFile
	if ok {
		var err error
		files, err = typed.ListAuthFiles(ctx)
		if err != nil {
			return nil, err
		}
	}
	found := map[string]bool{"codex": true}
	add := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if channel, err := NormalizeOAuthChannel(value); err == nil {
			found[channel] = true
		}
	}
	for _, file := range files {
		add(file.Provider)
	}
	if client, ok := k.CPAMP.(interface {
		ListOAuthChannels(context.Context) ([]string, error)
	}); ok {
		channels, err := client.ListOAuthChannels(ctx)
		if strict && err != nil {
			return nil, err
		}
		// Credential discovery still works when a configuration endpoint is
		// temporarily unavailable. Model/alias reads report their own errors.
		if err == nil {
			for _, channel := range channels {
				add(channel)
			}
		}
	}
	channels := []string{"codex"}
	others := []string{}
	for channel := range found {
		if channel != "codex" {
			others = append(others, channel)
		}
	}
	sort.Strings(others)
	return append(channels, others...), nil
}

func oauthChannelAliasRevision(channel string, aliases []cpamp.OAuthModelAlias) string {
	revision := oauthAliasRevision(aliases)
	if channel == "codex" {
		return revision
	} // Preserve legacy Codex forms.
	return security.SHA256(channel + "\x00" + revision)
}

func (k *Keys) removeChannelReasoningCap(ctx context.Context, channel, model string) error {
	if channel != "codex" && channel != "antigravity" {
		return nil
	}
	return k.removeOAuthReasoningCap(ctx, model, channel)
}

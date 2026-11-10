package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Account struct {
	LoggedIn bool   `json:"logged_in"`
	Email    string `json:"email,omitempty"`
}
type QuotaWindow struct {
	ID               string    `json:"id"`
	Label            string    `json:"label"`
	RemainingPercent int       `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at"`
}
type QuotaGroup struct {
	Label   string        `json:"label"`
	Windows []QuotaWindow `json:"windows"`
}
type QuotaSnapshot struct {
	Account   Account      `json:"account"`
	Groups    []QuotaGroup `json:"groups,omitempty"`
	UpdatedAt time.Time    `json:"updated_at"`
	Error     string       `json:"error,omitempty"`
	Stale     bool         `json:"stale,omitempty"`
}

func accountPath(home string) string { return filepath.Join(home, "agy", "antigravity-oauth-token") }
func readAccount(home string) Account {
	b, err := os.ReadFile(accountPath(home))
	if err != nil || len(b) > 256<<10 {
		return Account{}
	}
	var v struct {
		Email string          `json:"id_token_email"`
		Token json.RawMessage `json:"token"`
	}
	if json.Unmarshal(b, &v) != nil || len(v.Token) == 0 || string(v.Token) == "null" {
		return Account{}
	}
	return Account{LoggedIn: true, Email: v.Email}
}
func parseQuota(b []byte) ([]QuotaGroup, error) {
	var v struct {
		Command struct {
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						ID        string   `json:"id"`
						Window    string   `json:"window"`
						Remaining *float64 `json:"remaining_fraction"`
						Reset     string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal(b, &v) != nil || v.Command.Data.Groups == nil {
		return nil, errors.New("invalid AGY quota response")
	}
	groups := []QuotaGroup{}
	for _, g := range v.Command.Data.Groups {
		label := g.Name
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "gemini models":
			label = "Gemini"
		case "claude and gpt models":
			label = "Claude / GPT"
		}
		group := QuotaGroup{Label: label}
		for _, b := range g.Buckets {
			if b.Remaining == nil || *b.Remaining < 0 || *b.Remaining > 1 {
				continue
			}
			label := b.Window
			switch strings.ToLower(label) {
			case "weekly":
				label = "7D"
			case "five_hour", "five-hour", "5h":
				label = "5H"
			case "monthly":
				label = "月"
			}
			reset, _ := time.Parse(time.RFC3339, b.Reset)
			group.Windows = append(group.Windows, QuotaWindow{ID: b.ID, Label: label, RemainingPercent: int(math.Round(*b.Remaining * 100)), ResetAt: reset})
		}
		if len(group.Windows) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("AGY command output exceeded limit")
	}
	return b.Buffer.Write(p)
}

// Print mode uses the CLI's default home rather than ACP's --gemini-dir flags.
// Bind its default token directory to the same private account without making
// another token copy or changing the machine user's default CLI configuration.
func prepareQuotaHome(home string) (string, error) {
	root := filepath.Join(home, "quota-home")
	if err := os.MkdirAll(filepath.Join(root, ".gemini"), 0700); err != nil {
		return "", err
	}
	link := filepath.Join(root, ".gemini", "antigravity-cli")
	target := filepath.Join(home, "agy")
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		if err = os.Symlink(target, link); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	actual, err := filepath.EvalSymlinks(link)
	if err != nil || actual != target {
		return "", errors.New("AGY quota home does not refer to the configured account")
	}
	return root, nil
}
func commandEnv(key, value string) []string {
	env := []string{}
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if !strings.EqualFold(name, key) {
			env = append(env, v)
		}
	}
	return append(env, key+"="+value)
}
func (m *Manager) readQuota(ctx context.Context) (QuotaSnapshot, error) {
	if m.quotaReadTest != nil {
		return m.quotaReadTest(ctx)
	}
	result := QuotaSnapshot{Account: readAccount(m.cfg.Home), UpdatedAt: time.Now()}
	if !result.Account.LoggedIn {
		return result, nil
	}
	home, err := prepareQuotaHome(m.cfg.Home)
	if err != nil {
		return result, err
	}
	c, done := context.WithTimeout(ctx, 45*time.Second)
	defer done()
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-c.Done():
		return result, c.Err()
	}
	cmd := exec.CommandContext(c, m.cfg.Command, "-p", "/usage", "--output-format", "json")
	cmd.Dir = filepath.Join(m.cfg.Home, "workspace")
	cmd.Env = commandEnv("HOME", home)
	configureProcess(cmd)
	cmd.Cancel = func() error { killProcess(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	output := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		return result, errors.New("AGY quota command failed")
	}
	result.Account = readAccount(m.cfg.Home)
	result.Groups, err = parseQuota(output.Bytes())
	return result, err
}
func (m *Manager) Quota(ctx context.Context, force bool) (QuotaSnapshot, error) {
	for {
		m.quotaMu.Lock()
		ttl := 10 * time.Minute
		if m.quota.Error != "" {
			ttl = 30 * time.Second
		}
		if !force && !m.quotaAt.IsZero() && time.Since(m.quotaAt) < ttl {
			v := m.quota
			m.quotaMu.Unlock()
			return v, nil
		}
		if flight := m.quotaFlight; flight != nil {
			m.quotaMu.Unlock()
			select {
			case <-flight:
				force = false
				continue
			case <-ctx.Done():
				return QuotaSnapshot{}, ctx.Err()
			}
		}
		flight := make(chan struct{})
		generation := m.quotaGeneration
		m.quotaFlight = flight
		m.quotaMu.Unlock()
		value, err := m.readQuota(ctx)
		m.quotaMu.Lock()
		if generation != m.quotaGeneration {
			m.quotaFlight = nil
			close(flight)
			m.quotaMu.Unlock()
			force = false
			continue
		}
		if err != nil {
			value = m.quota
			value.Account = readAccount(m.cfg.Home)
			value.Error = "官方额度暂时无法读取"
			value.Stale = len(value.Groups) > 0
		}
		m.quota = value
		m.quotaAt = time.Now()
		m.quotaFlight = nil
		close(flight)
		m.quotaMu.Unlock()
		return value, nil
	}
}
func (m *Manager) invalidateQuota() {
	m.quotaMu.Lock()
	m.quota = QuotaSnapshot{}
	m.quotaAt = time.Time{}
	m.quotaGeneration++
	m.quotaMu.Unlock()
}
func (m *Manager) quotaHTTP(w http.ResponseWriter, r *http.Request) {
	v, err := m.Quota(r.Context(), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		WriteError(w, fail(503, "quota_unavailable", "额度读取未完成"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

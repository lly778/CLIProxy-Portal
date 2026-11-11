package agy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type loginJob struct {
	mu                   sync.Mutex
	id, owner, home, url string
	cmd                  *exec.Cmd
	in                   io.WriteCloser
	cancel               context.CancelFunc
	done                 chan struct{}
	urlReady             chan struct{}
	err                  error
	submitted            bool
	expires              time.Time
}
type LoginChallenge struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func googleAuthURL(line string) string {
	start := strings.Index(line, "https://accounts.google.com/")
	if start < 0 {
		return ""
	}
	text := line[start:]
	if i := strings.IndexAny(text, " \t\r\n\x1b\"<>"); i >= 0 {
		text = text[:i]
	}
	u, err := url.Parse(text)
	if err != nil || u.Scheme != "https" || u.Host != "accounts.google.com" || !strings.HasPrefix(u.Path, "/o/oauth2/") || u.Query().Get("response_type") != "code" {
		return ""
	}
	return u.String()
}
func (m *Manager) startLogin(ctx context.Context, owner string) (LoginChallenge, error) {
	m.authMu.Lock()
	if old := m.authJob; old != nil {
		select {
		case <-old.done:
			old.cancel()
		default:
			if old.owner != owner {
				m.authMu.Unlock()
				return LoginChallenge{}, fail(409, "login_busy", "已有管理员正在登录账号")
			}
			old.mu.Lock()
			v := LoginChallenge{old.id, old.url, old.expires}
			old.mu.Unlock()
			m.authMu.Unlock()
			if v.URL == "" {
				return v, fail(409, "login_starting", "授权页面正在准备，请稍后重试")
			}
			return v, nil
		}
	}
	home, err := os.MkdirTemp(m.cfg.Home, "auth-")
	if err != nil {
		m.authMu.Unlock()
		return LoginChallenge{}, err
	}
	base := m.life
	if base == nil {
		base = context.Background()
	}
	life, cancel := context.WithTimeout(base, 10*time.Minute)
	cmd := exec.CommandContext(life, m.cfg.Command, "acp", "--gemini-dir", home, "--app-data-dir", "agy", "--login")
	if m.authCommandTest != nil {
		cmd = m.authCommandTest(life, home)
	}
	cmd.Dir = filepath.Join(m.cfg.Home, "workspace")
	configureProcess(cmd)
	cmd.Cancel = func() error { killProcess(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	job := &loginJob{id: newID("login_"), owner: owner, home: home, cmd: cmd, cancel: cancel, done: make(chan struct{}), urlReady: make(chan struct{}), expires: time.Now().Add(10 * time.Minute)}
	job.in, err = cmd.StdinPipe()
	var stdout, stderr io.ReadCloser
	if err == nil {
		stdout, err = cmd.StdoutPipe()
	}
	if err == nil {
		stderr, err = cmd.StderrPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		m.removeLoginHome(home)
		m.authMu.Unlock()
		return LoginChallenge{}, fail(503, "login_unavailable", "无法启动官方账号登录")
	}
	m.authJob = job
	m.authMu.Unlock()
	read := func(r io.Reader) {
		scan := bufio.NewScanner(r)
		scan.Buffer(make([]byte, 4096), 64<<10)
		for scan.Scan() {
			if u := googleAuthURL(scan.Text()); u != "" {
				job.mu.Lock()
				if job.url == "" {
					job.url = u
					close(job.urlReady)
				}
				job.mu.Unlock()
			}
		}
	}
	go read(stdout)
	go read(stderr)
	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		job.err = err
		job.mu.Unlock()
		close(job.done)
		// Successful credentials remain isolated until the administrator confirms
		// them. Cancelled or abandoned jobs are removed when their lease expires.
		<-life.Done()
		m.removeLoginHome(home)
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-job.urlReady:
		job.mu.Lock()
		v := LoginChallenge{job.id, job.url, job.expires}
		job.mu.Unlock()
		return v, nil
	case <-job.done:
		cancel()
		return LoginChallenge{}, fail(503, "login_failed", "官方登录未能启动，请稍后重试")
	case <-timer.C:
		cancel()
		return LoginChallenge{}, fail(504, "login_timeout", "准备授权页面超时，请重试")
	case <-ctx.Done():
		cancel()
		return LoginChallenge{}, ctx.Err()
	}
}
func (m *Manager) removeLoginHome(home string) {
	parent := filepath.Clean(m.cfg.Home)
	if filepath.Dir(filepath.Clean(home)) == parent && strings.HasPrefix(filepath.Base(home), "auth-") {
		_ = os.RemoveAll(home)
	}
}
func loginCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if strings.HasPrefix(code, "https://") {
		u, err := url.Parse(code)
		if err != nil || (u.Host != "antigravity.google" && u.Host != "antigravity.google.com") {
			return "", errors.New("invalid callback")
		}
		code = u.Query().Get("code")
	}
	if len(code) < 8 || len(code) > 8192 || strings.ContainsAny(code, "\r\n\x00") {
		return "", errors.New("invalid authorization code")
	}
	return code, nil
}
func (m *Manager) finishLogin(ctx context.Context, owner, id, raw string) (Account, error) {
	code, err := loginCode(raw)
	if err != nil {
		return Account{}, fail(400, "invalid_code", "请输入授权码或授权完成后的回调地址")
	}
	m.authMu.Lock()
	job := m.authJob
	m.authMu.Unlock()
	if job == nil || job.id != id || job.owner != owner || time.Now().After(job.expires) {
		return Account{}, fail(409, "login_expired", "本次授权已失效，请重新登录")
	}
	job.mu.Lock()
	if !job.submitted {
		job.submitted = true
		_, err = io.WriteString(job.in, code+"\n")
	}
	job.mu.Unlock()
	if err != nil {
		return Account{}, fail(400, "login_failed", "授权码未能提交，请重新登录")
	}
	select {
	case <-job.done:
	case <-ctx.Done():
		return Account{}, ctx.Err()
	}
	job.mu.Lock()
	err = job.err
	job.mu.Unlock()
	account := readAccount(job.home)
	if err != nil || !account.LoggedIn {
		job.cancel()
		return Account{}, fail(400, "login_failed", "官方授权失败，当前账号保持不变，请重新登录")
	}
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-ctx.Done():
		return Account{}, ctx.Err()
	}
	m.authMu.Lock()
	defer m.authMu.Unlock()
	if m.authJob != job {
		return Account{}, fail(409, "login_expired", "本次授权已被替换，请重新登录")
	}
	data, err := os.ReadFile(accountPath(job.home))
	if err != nil || len(data) > 256<<10 {
		return Account{}, fail(503, "login_failed", "无法保存官方登录信息")
	}
	tmp, err := os.CreateTemp(filepath.Join(m.cfg.Home, "agy"), ".account-")
	if err != nil {
		return Account{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Account{}, err
	}
	if err = m.resetAccountSessions(); err != nil {
		return Account{}, err
	}
	if err = os.Rename(name, accountPath(m.cfg.Home)); err != nil {
		return Account{}, err
	}
	m.invalidateQuota()
	m.authJob = nil
	job.cancel()
	return account, nil
}
func (m *Manager) resetAccountSessions() error {
	if err := m.clearResponseReplays(); err != nil {
		return err
	}
	if m.disk != nil {
		tx, err := m.disk.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`UPDATE sessions SET ready=0`); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM responses`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	p := m.proc
	m.proc = nil
	all := m.sessions
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range all {
		s.cancel()
	}
	if p != nil {
		p.close()
	}
	return nil
}
func (m *Manager) logout(ctx context.Context) error {
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.authMu.Lock()
	defer m.authMu.Unlock()
	if err := m.resetAccountSessions(); err != nil {
		return err
	}
	if err := os.Remove(accountPath(m.cfg.Home)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if m.authJob != nil {
		m.authJob.cancel()
		m.authJob = nil
	}
	m.invalidateQuota()
	return nil
}
func (m *Manager) cancelLogin(owner, id string) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	if job := m.authJob; job != nil && job.owner == owner && job.id == id {
		job.cancel()
		m.authJob = nil
	}
}
func (m *Manager) loginCancelHTTP(w http.ResponseWriter, r *http.Request) {
	owner, err := loginOwner(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&v) != nil {
		WriteError(w, fail(400, "invalid_request", "授权信息无效"))
		return
	}
	m.cancelLogin(owner, v.ID)
	w.WriteHeader(http.StatusNoContent)
}
func loginOwner(r *http.Request) (string, error) {
	owner := r.Header.Get("X-Portal-Owner")
	if owner == "" || len(owner) > 256 {
		return "", fail(401, "owner_required", "缺少管理员身份")
	}
	return owner, nil
}
func (m *Manager) loginStartHTTP(w http.ResponseWriter, r *http.Request) {
	owner, err := loginOwner(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	v, err := m.startLogin(r.Context(), owner)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (m *Manager) loginFinishHTTP(w http.ResponseWriter, r *http.Request) {
	owner, err := loginOwner(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body) != nil {
		WriteError(w, fail(400, "invalid_request", "授权信息无效"))
		return
	}
	v, err := m.finishLogin(r.Context(), owner, body.ID, body.Code)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (m *Manager) logoutHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := loginOwner(r); err != nil {
		WriteError(w, err)
		return
	}
	if err := m.logout(r.Context()); err != nil {
		WriteError(w, fail(503, "logout_failed", "退出账号未能完成，请重试"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

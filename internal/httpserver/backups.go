package httpserver

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/backup"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/webui"
)

func (s *Server) backupView() webui.BackupView {
	v := webui.BackupView{Available: s.Cfg.BackupStateDir != "", LastSuccess: "尚无成功备份", NextRun: "未启用", Message: "尚未配置"}
	if !v.Available {
		v.Warning = "服务器尚未设置备份状态目录"
		return v
	}
	dir := s.Cfg.BackupStateDir
	c, err := backup.LoadConfig(dir)
	if err != nil {
		v.Warning = "后台备份配置无法读取"
		return v
	}
	_, err = backup.ReadKey(filepath.Join(dir, "recovery.key"))
	v.KeyReady = err == nil
	_, err = backup.LoadToken(dir)
	v.Repository, v.KeySaved, v.TokenReady = c.Repository, c.KeySaved, err == nil
	v.Automatic, v.Weekday, v.Time = c.Enabled || c.Instance == "", c.Weekday, fmt.Sprintf("%02d:%02d", c.Hour, c.Minute)
	v.Configured = backup.Validate(c) == nil && v.KeyReady && err == nil
	if v.Configured {
		v.Message = "等待备份"
		if !c.Enabled {
			v.Message = "定时备份已关闭"
		}
	}
	status, err := backup.LoadStatus(dir)
	if err != nil {
		v.Warning = "备份状态无法读取"
		return v
	}
	v.Running, v.Pending = status.Running, backup.Pending(dir)
	if status.Message != "" && status.Repository == c.Repository {
		v.Message = status.Message
	}
	if !status.LastSuccessAt.IsZero() && status.SuccessRepository == c.Repository {
		v.LastSuccess = s.formatTime(status.LastSuccessAt)
		v.Size = fmt.Sprintf("%.1f MB", float64(status.Size)/(1024*1024))
	}
	if v.Pending {
		v.Message = "备份已排队"
	}
	if c.Enabled {
		now := time.Now()
		next := backup.NextScheduledAt(c, now)
		v.NextRun = next.Format("2006-01-02 15:04") + "（北京时间）"
		if backup.Due(c, status, now) {
			v.NextRun = "等待执行本周备份"
		}
	}
	restore, err := backup.LoadRestoreStatus(dir)
	if err != nil {
		v.Warning = "恢复状态无法读取"
		return v
	}
	v.RestoreMessage, v.RestoreRollback = restore.Message, restore.Rollback
	if backup.DatabaseRecoveryPending(dir) {
		v.Running = true
		v.Warning = "数据库恢复事务尚未完成，请检查后台任务"
	}
	if backup.ManagerBackupPending(dir) {
		v.Running = true
		v.Warning = "CPAMP 快照暂停尚未结束，后台任务将恢复服务；请勿同时恢复数据库"
	}
	if restore.Running {
		v.Running = true
	}
	if backup.RestorePending(dir) {
		v.Pending = true
		v.RestoreMessage = "恢复已排队"
	}
	seen := backup.RunnerSeen(dir)
	if !v.Running && (seen.IsZero() || time.Since(seen) > 3*time.Minute) {
		v.Warning = "宿主机备份任务未连接，请检查后台任务"
	}
	if v.Running {
		if _, err := os.Stat(filepath.Join(dir, "runner.lock")); err != nil {
			v.Warning = "任务可能异常中断，请检查后台状态"
		}
	}
	return v
}

// Multipart credentials are small fields sent before the streamed encrypted
// upload. Authentication is checked before any file is written to disk.
func (s *Server) backupAuthorize(w http.ResponseWriter, r *http.Request, csrf, password string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if csrf == "" || !security.VerifyCSRF(s.Secret, s.sessionToken(r), csrf) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return false
	}
	if s.Cfg.BackupStateDir == "" {
		s.errorPage(w, r, 503, "服务器尚未配置备份功能", nil)
		return false
	}
	u := currentUser(r)
	if !s.limit.Allow("backup-password:"+u.ID, 10, 10*time.Minute, 15*time.Minute) {
		s.errorPage(w, r, 429, "尝试过于频繁，请稍后重试", nil)
		return false
	}
	if !security.VerifyPassword(u.PasswordHash, password) {
		s.errorPage(w, r, 403, "管理员密码不正确", nil)
		return false
	}
	return true
}

func (s *Server) backupIdle() bool {
	dir := s.Cfg.BackupStateDir
	b, e1 := backup.LoadStatus(dir)
	r, e2 := backup.LoadRestoreStatus(dir)
	_, lockErr := os.Stat(filepath.Join(dir, "runner.lock"))
	return e1 == nil && e2 == nil && !b.Running && !r.Running && !backup.Pending(dir) && !backup.RestorePending(dir) && !backup.DatabaseRecoveryPending(dir) && !backup.ManagerBackupPending(dir) && os.IsNotExist(lockErr)
}

func (s *Server) backupSettingsAuthorize(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, 400, "备份设置请求无效", nil)
		return false
	}
	return s.backupAuthorize(w, r, r.PostForm.Get("csrf_token"), r.PostForm.Get("admin_password"))
}

func (s *Server) adminBackupSettings(w http.ResponseWriter, r *http.Request) {
	if !s.backupSettingsAuthorize(w, r) {
		return
	}
	allowed := map[string]bool{"csrf_token": true, "admin_password": true, "repository": true, "github_token": true, "key_saved": true, "enabled": true, "weekday": true, "backup_time": true}
	for name, values := range r.PostForm {
		if !allowed[name] || len(values) != 1 {
			s.errorPage(w, r, 400, "备份设置字段无效", nil)
			return
		}
	}
	weekday, weekdayErr := strconv.Atoi(r.PostForm.Get("weekday"))
	clock, clockErr := time.Parse("15:04", r.PostForm.Get("backup_time"))
	if weekdayErr != nil || clockErr != nil || clock.Format("15:04") != r.PostForm.Get("backup_time") || weekday < 0 || weekday > 6 || (r.PostForm.Get("enabled") != "" && r.PostForm.Get("enabled") != "1") {
		s.errorPage(w, r, 400, "请选择备份星期和北京时间（00:00–23:59）", nil)
		return
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if !s.backupIdle() {
		s.errorPage(w, r, 409, "已有备份或恢复任务正在执行或等待执行", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	repository := strings.TrimSpace(r.PostForm.Get("repository"))
	err := backup.WithIdleState(s.Cfg.BackupStateDir, func() error {
		return backup.ConfigureSettings(ctx, s.Cfg.BackupStateDir, repository, r.PostForm.Get("github_token"), r.PostForm.Get("key_saved") == "1", r.PostForm.Get("enabled") == "1", weekday, clock.Hour(), clock.Minute())
	})
	if err != nil {
		s.errorPage(w, r, 400, err.Error(), nil)
		return
	}
	s.audit(r, currentUser(r), "backup.settings.update", repository, "修改备份设置；令牌和恢复密钥不记入日志")
	http.Redirect(w, r, "/admin/system?msg="+url.QueryEscape("备份设置已保存")+"#backup", http.StatusSeeOther)
}

func (s *Server) adminBackupKey(w http.ResponseWriter, r *http.Request) {
	if !s.backupSettingsAuthorize(w, r) {
		return
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if !s.backupIdle() {
		s.errorPage(w, r, 409, "已有备份或恢复任务正在执行或等待执行", nil)
		return
	}
	var key []byte
	err := backup.WithIdleState(s.Cfg.BackupStateDir, func() error {
		var err error
		key, err = backup.ReadKey(filepath.Join(s.Cfg.BackupStateDir, "recovery.key"))
		if os.IsNotExist(err) {
			config, configErr := backup.LoadConfig(s.Cfg.BackupStateDir)
			status, statusErr := backup.LoadStatus(s.Cfg.BackupStateDir)
			if configErr != nil || statusErr != nil || config.Instance != "" || config.Repository != "" || !status.LastSuccessAt.IsZero() {
				return errors.New("已配置的恢复密钥缺失，不能自动更换")
			}
			key, err = backup.EnsureKey(s.Cfg.BackupStateDir)
		}
		return err
	})
	if err != nil {
		s.errorPage(w, r, 400, "恢复密钥无法下载，请检查后台状态", nil)
		return
	}
	s.audit(r, currentUser(r), "backup.key.download", "backup", "下载备份恢复密钥；密钥内容不记入日志")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="portal-recovery.key"`)
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_, _ = io.WriteString(w, hex.EncodeToString(key)+"\n")
}

func (s *Server) adminBackupRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, 400, "备份请求无效", nil)
		return
	}
	if !s.backupAuthorize(w, r, r.PostForm.Get("csrf_token"), r.PostForm.Get("admin_password")) {
		return
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	dir := s.Cfg.BackupStateDir
	c, err := backup.LoadConfig(dir)
	if err != nil || backup.Validate(c) != nil {
		s.errorPage(w, r, 400, "后台备份尚未配置完成", nil)
		return
	}
	if !s.backupIdle() {
		s.errorPage(w, r, 409, "已有备份或恢复任务正在执行或等待执行", nil)
		return
	}
	if _, err = backup.ReadKey(filepath.Join(dir, "recovery.key")); err != nil {
		s.errorPage(w, r, 400, "恢复密钥不可用", nil)
		return
	}
	if _, err = backup.LoadToken(dir); err != nil {
		s.errorPage(w, r, 400, "后台备份凭据不可用", nil)
		return
	}
	if err = backup.Request(dir); err != nil {
		s.errorPage(w, r, 409, err.Error(), nil)
		return
	}
	s.audit(r, currentUser(r), "backup.request", "backup", "请求一次加密备份，由宿主机任务执行")
	http.Redirect(w, r, "/admin/system#backup", http.StatusSeeOther)
}

func (s *Server) adminBackupRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, backup.MaxEncryptedSize+(16<<10))
	reader, err := r.MultipartReader()
	if err != nil {
		s.errorPage(w, r, 400, "请选择备份文件", nil)
		return
	}
	values := map[string]string{}
	for i := 0; i < 6; i++ {
		part, err := reader.NextPart()
		if err != nil {
			s.errorPage(w, r, 400, "恢复请求不完整", nil)
			return
		}
		name := part.FormName()
		if name == "backup_file" {
			if !s.backupAuthorize(w, r, values["csrf_token"], values["admin_password"]) {
				return
			}
			mode := values["restore_mode"]
			if values["confirm"] != "replace-current-data" || (mode != backup.DatabaseRestoreMode && mode != backup.RebuildRestoreMode) {
				s.errorPage(w, r, 400, "请选择恢复方式并明确确认覆盖数据", nil)
				return
			}
			key, keyErr := backup.ReadKey(filepath.Join(s.Cfg.BackupStateDir, "recovery.key"))
			if values["recovery_key"] != "" {
				key, keyErr = hex.DecodeString(strings.TrimSpace(values["recovery_key"]))
			}
			if keyErr != nil || len(key) != 32 {
				s.errorPage(w, r, 400, "恢复密钥不可用，请在后台配置或提供恢复密钥", nil)
				return
			}
			s.backupMu.Lock()
			defer s.backupMu.Unlock()
			if !s.backupIdle() {
				s.errorPage(w, r, 409, "已有备份或恢复任务正在执行或等待执行", nil)
				return
			}
			file := &finalMultipartFile{file: part, reader: reader}
			if err = backup.RequestRestore(s.Cfg.BackupStateDir, file, key, mode); err != nil {
				s.errorPage(w, r, 400, "备份文件无效或无法提交恢复任务", nil)
				return
			}
			detail := "管理员确认仅覆盖门户数据库；后台校验、停服替换、重启检查，失败回退"
			action := "backup.restore.request"
			if mode == backup.RebuildRestoreMode {
				action = "backup.rebuild.request"
				detail = "管理员确认重建 CPA、CPAMP 并恢复管理密钥、部署参数、门户及 CPAMP 数据库、data.key、config.yaml 及 auths；失败一起回退，不包含归档文件"
			}
			s.audit(r, currentUser(r), action, "backup", detail)
			http.Redirect(w, r, "/admin/system#backup", http.StatusSeeOther)
			return
		}
		if name != "csrf_token" && name != "admin_password" && name != "confirm" && name != "recovery_key" && name != "restore_mode" {
			s.errorPage(w, r, 400, "恢复请求字段无效", nil)
			return
		}
		if _, exists := values[name]; exists {
			s.errorPage(w, r, 400, "恢复请求字段重复", nil)
			return
		}
		body, err := io.ReadAll(io.LimitReader(part, 1025))
		if err != nil || len(body) > 1024 {
			s.errorPage(w, r, 400, "恢复请求字段过长", nil)
			return
		}
		values[name] = string(body)
		_ = part.Close()
	}
	s.errorPage(w, r, 400, "请选择备份文件", nil)
}

// Verify the last multipart boundary before publishing a queued restore job.
type finalMultipartFile struct {
	file   io.Reader
	reader *multipart.Reader
}

func (f *finalMultipartFile) Read(p []byte) (int, error) {
	n, err := f.file.Read(p)
	if err == io.EOF {
		if _, endErr := f.reader.NextPart(); endErr != io.EOF {
			return n, errors.New("恢复请求包含额外或不完整数据")
		}
	}
	return n, err
}

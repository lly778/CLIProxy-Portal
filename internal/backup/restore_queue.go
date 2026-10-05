package backup

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// GitHub Release assets must be smaller than 2 GiB. Decoded SQLite snapshots
// and archives have a separate 8 GiB bound because they compress well.
const MaxEncryptedSize int64 = (2 << 30) - 1

type RestoreStatus struct {
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Message    string    `json:"message"`
	Rollback   string    `json:"rollback"`
}

type restoreRequest struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
}

func LoadRestoreStatus(dir string) (RestoreStatus, error) {
	var s RestoreStatus
	err := readJSON(filepath.Join(dir, "restore-status.json"), &s)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	return s, err
}

func RestorePending(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "restore-pending"))
	return err == nil
}

// RequestRestore stores only an encrypted upload and a private key snapshot.
// The client cannot select a destination. The host policy fixes the live DB.
func RequestRestore(dir string, archive io.Reader, key []byte, mode string) error {
	if mode != DatabaseRestoreMode && mode != RebuildRestoreMode {
		return errors.New("恢复模式无效")
	}
	if len(key) != 32 {
		return errors.New("恢复密钥无效")
	}
	if RestorePending(dir) {
		return errors.New("已有恢复任务等待执行")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(dir, ".restore-upload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	f, err := os.OpenFile(filepath.Join(work, "backup.cbackup"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	header := make([]byte, len(magic))
	_, err = io.ReadFull(archive, header)
	if err != nil || string(header) != magic {
		_ = f.Close()
		return errors.New("请选择加密的 .cbackup 备份文件")
	}
	_, err = f.Write(header)
	if err == nil {
		var n int64
		n, err = io.Copy(f, io.LimitReader(archive, MaxEncryptedSize-int64(len(header))+1))
		if n+int64(len(header)) > MaxEncryptedSize {
			err = errors.New("备份文件超过大小限制")
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = atomicWrite(filepath.Join(work, "recovery.key"), []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(work, "request.json"), restoreRequest{Version: 2, Mode: mode}); err != nil {
		return err
	}
	return os.Rename(work, filepath.Join(dir, "restore-pending"))
}

func restoreTick(ctx context.Context, dir, policyFile string, now time.Time, newController func(PortalRestorePolicy, string) portalController) error {
	lock := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("备份或恢复任务正在执行，请先检查宿主机任务")
	}
	_ = f.Close()
	defer os.Remove(lock)
	if DatabaseRecoveryPending(dir) {
		p, err := LoadRestorePolicy(policyFile)
		if err != nil {
			return err
		}
		var j databaseTransaction
		if err = readJSON(filepath.Join(dir, transactionName, "transaction.json"), &j); err != nil {
			return err
		}
		controller := newController(p, j.Mode)
		if d, ok := controller.(*dockerPortal); ok && j.Mode == RebuildRestoreMode {
			// The approved on-host profile must still match before recovery runs.
			if err = d.validateProfile(); err != nil {
				return err
			}
		}
		return recoverDatabaseTransaction(ctx, dir, p, controller)
	}
	work, err := os.MkdirTemp(dir, ".restore-job-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	request := filepath.Join(work, "request")
	if err = os.Rename(filepath.Join(dir, "restore-pending"), request); err != nil {
		return err
	}
	s := RestoreStatus{Running: true, StartedAt: now, Message: "正在校验恢复备份"}
	if err = writeJSON(filepath.Join(dir, "restore-status.json"), s); err != nil {
		return err
	}
	// A private parent prevents the app UID from reading restored upstream
	// secrets or substituting destinations while the privileged host runs.
	jobErr := validateRestoreRequest(request)
	var job restoreRequest
	if jobErr == nil {
		jobErr = readJSON(filepath.Join(request, "request.json"), &job)
	}
	var policy PortalRestorePolicy
	if jobErr == nil {
		policy, jobErr = LoadRestorePolicy(policyFile)
	}
	if jobErr == nil {
		jobErr = Restore(ctx, filepath.Join(request, "backup.cbackup"), filepath.Join(request, "recovery.key"), filepath.Join(work, "files"))
	}
	if jobErr == nil {
		controller := newController(policy, job.Mode)
		if job.Mode == RebuildRestoreMode {
			preparer, ok := controller.(interface {
				Prepare(context.Context, string) error
			})
			if !ok {
				jobErr = errors.New("后台重建任务未配置")
			} else {
				jobErr = preparer.Prepare(ctx, filepath.Join(work, "files"))
			}
		}
		if jobErr == nil {
			s.Rollback, jobErr = replacePortalDatabase(ctx, dir, filepath.Join(work, "files"), policy, controller, job.Mode)
		}
	}
	s.Running, s.FinishedAt = false, time.Now().UTC()
	if jobErr != nil {
		s.Message = "恢复失败：" + jobErr.Error()
	} else {
		s.Message = "门户数据库已恢复，服务健康；请重新登录"
		if job.Mode == RebuildRestoreMode {
			s.Message = "CPA、CPAMP 已重建，门户及 CPAMP 数据库、data.key、管理密钥、config.yaml 和 auths 已恢复；请重新登录"
		}
	}
	if err = writeJSON(filepath.Join(dir, "restore-status.json"), s); err != nil {
		return err
	}
	return jobErr
}

func validateRestoreRequest(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("恢复请求目录无效")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		return errors.New("恢复请求文件数量无效")
	}
	for _, name := range []string{"backup.cbackup", "recovery.key", "request.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxEncryptedSize {
			return errors.New("恢复请求必须包含普通备份文件与密钥，不接受链接")
		}
	}
	var request restoreRequest
	if err := readJSON(filepath.Join(dir, "request.json"), &request); err != nil || request.Version != 2 || (request.Mode != DatabaseRestoreMode && request.Mode != RebuildRestoreMode) {
		return errors.New("未明确确认覆盖当前数据库，请重新提交恢复请求")
	}
	return nil
}

package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Due(c Config, s Status, now time.Time) bool {
	if !c.Enabled || c.Weekday < 0 || c.Weekday > 6 || c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 {
		return false
	}
	scheduled := ScheduledAt(c, now)
	return !now.Before(scheduled) && (s.Repository != c.Repository || s.LastAttemptDay < scheduled.Format("2006-01-02"))
}

// ScheduledAt uses calendar weeks starting Monday in Beijing time. A missed
// run catches up later in that same week, but never runs more than one scheduled
// attempt per week (including failure). Manual requests remain independent.
func ScheduledAt(c Config, now time.Time) time.Time {
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := now.In(zone)
	monday := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, -(int(local.Weekday())+6)%7)
	return monday.AddDate(0, 0, (c.Weekday+6)%7).Add(time.Duration(c.Hour)*time.Hour + time.Duration(c.Minute)*time.Minute)
}

func NextScheduledAt(c Config, now time.Time) time.Time {
	next := ScheduledAt(c, now)
	if !next.After(now) {
		next = next.AddDate(0, 0, 7)
	}
	return next
}

// Tick is invoked by a once-per-minute host timer. The lock also protects
// against concurrent CLI runs; an interrupted job requires operator review.
func Tick(ctx context.Context, dir, sourcesFile string, restorePolicy ...string) error {
	return tick(ctx, dir, sourcesFile, time.Now().UTC(), NewGitHub, restorePolicy...)
}

func tick(ctx context.Context, dir, sourcesFile string, now time.Time, newClient func(string) *GitHub, restorePolicy ...string) error {
	policyFile := filepath.Join(filepath.Dir(sourcesFile), "restore.json")
	if len(restorePolicy) > 0 && restorePolicy[0] != "" {
		policyFile = restorePolicy[0]
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "heartbeat.json"), now); err != nil {
		return err
	}
	if ManagerBackupPending(dir) {
		return recoverManagerSnapshot(ctx, dir, policyFile)
	}
	if RestorePending(dir) || DatabaseRecoveryPending(dir) {
		return restoreTick(ctx, dir, policyFile, now, func(p PortalRestorePolicy, mode string) portalController {
			return &dockerPortal{policy: p, rebuild: mode == RebuildRestoreMode}
		})
	}
	c, err := LoadConfig(dir)
	if err != nil {
		return errors.New("备份设置无法读取")
	}
	if c.Repository == "" {
		return nil
	}
	s, err := LoadStatus(dir)
	if err != nil {
		return errors.New("备份状态无法读取")
	}
	manual := Pending(dir)
	if !manual && !Due(c, s, now) {
		return nil
	}
	if err = Validate(c); err != nil {
		return err
	}
	lock := filepath.Join(dir, "runner.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return errors.New("备份任务锁已存在；若任务中断，请先检查宿主机任务状态")
	}
	if err != nil {
		return err
	}
	_ = f.Close()
	defer os.Remove(lock)
	// A settings save can finish between the initial due check and acquisition.
	// Never pair a stale repository configuration with a newly saved token.
	c, err = LoadConfig(dir)
	if err != nil {
		return errors.New("备份设置无法读取")
	}
	s, err = LoadStatus(dir)
	if err != nil {
		return errors.New("备份状态无法读取")
	}
	manual = Pending(dir)
	if !manual && !Due(c, s, now) {
		return nil
	}
	if err = Validate(c); err != nil {
		return err
	}
	work, err := os.MkdirTemp(dir, ".job-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if manual {
		if err = os.Rename(filepath.Join(dir, "pending"), filepath.Join(work, "request")); err != nil {
			return err
		}
	}
	zone, _ := time.LoadLocation("Asia/Shanghai")
	s.Running = true
	s.Repository = c.Repository
	s.StartedAt = now
	s.FinishedAt = time.Time{}
	s.Phase = "preflight"
	s.StageDurationMS = make(map[string]int64)
	s.TotalDurationMS = 0
	s.LastAttemptDay = now.In(zone).Format("2006-01-02")
	s.Message = "正在生成并上传加密备份"
	if err = writeJSON(filepath.Join(dir, "status.json"), s); err != nil {
		return err
	}
	started := time.Now()
	saveProgress := func() {
		if e := writeJSON(filepath.Join(dir, "status.json"), s); e != nil {
			log.Print("backup progress state could not be saved")
		}
	}
	stages := &stageHooks{
		begin: func(name string) {
			s.Phase = name
			messages := map[string]string{"preflight": "正在检查备份条件", "snapshot": "正在生成数据库和配置快照", "compress_encrypt": "正在压缩并加密备份", "local_verify": "正在进行本地恢复验证", "upload": "正在上传加密备份", "download_verify": "正在下载校验远端备份", "publish": "正在发布已验证备份", "retention": "正在清理超过保留数量的旧备份"}
			if message, ok := messages[name]; ok {
				s.Message = message
			}
			saveProgress()
		},
		finish: func(name string, elapsed time.Duration) {
			s.StageDurationMS[name] += elapsed.Milliseconds()
			log.Printf("backup stage=%s duration_ms=%d", name, elapsed.Milliseconds())
			saveProgress()
		},
	}
	jobErr := func() error {
		finishPreflight := stages.start("preflight")
		defer finishPreflight()
		token, e := LoadToken(dir)
		if e != nil {
			return errors.New("GitHub 令牌未配置或无法读取")
		}
		key, e := ReadKey(filepath.Join(dir, "recovery.key"))
		if e != nil {
			return errors.New("恢复密钥未配置或无法读取")
		}
		sources, e := LoadSources(sourcesFile)
		if e != nil {
			return e
		}
		statePath, e := filepath.Abs(dir)
		if e != nil {
			return e
		}
		for _, source := range sources {
			p, e := filepath.Abs(source.Path)
			if e != nil {
				return e
			}
			if within(p, statePath) || (source.Directory && within(statePath, p)) {
				return errors.New("备份来源不能包含备份状态目录、GitHub 令牌或恢复密钥")
			}
		}
		g := newClient(token)
		if e = g.CheckPrivate(ctx, c.Repository); e != nil {
			return e
		}
		var images map[string]string
		var policy PortalRestorePolicy
		if _, statErr := os.Lstat(policyFile); statErr == nil {
			p, err := LoadRestorePolicy(policyFile)
			if err != nil {
				return err
			}
			if p.UpstreamContainer != "" || p.ManagerContainer != "" {
				if err = p.validate(RebuildRestoreMode); err != nil {
					return err
				}
				policy = p
				images, err = captureImages(ctx, p)
				if err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(statErr) {
			return errors.New("后台重建策略无法读取")
		}
		manager, e := managerSources(sources, policy)
		if e != nil {
			return e
		}
		if e = backupSpace(sources, work); e != nil {
			return e
		}
		var freeze func(func() error) error
		if manager {
			driver := &dockerPortal{policy: policy}
			freeze = func(collect func() error) error { return driver.withManagerSnapshot(ctx, dir, collect) }
		}
		finishPreflight()
		archive, _, e := createArchive(ctx, sources, work, key, images, freeze, stages)
		if e != nil {
			return e
		}
		// Prove decryption, hashes and SQLite integrity before uploading.
		finishVerify := stages.start("local_verify")
		e = Restore(ctx, archive, filepath.Join(dir, "recovery.key"), filepath.Join(work, "verified"))
		finishVerify()
		if e != nil {
			return fmt.Errorf("本地恢复验证失败: %w", e)
		}
		random := make([]byte, 4)
		if _, e = rand.Read(random); e != nil {
			return e
		}
		tag := releaseTagPrefix + c.Instance + "-" + now.Format("20060102T150405Z") + "-" + hex.EncodeToString(random)
		var link, sha string
		var size int64
		link, size, sha, e = g.upload(ctx, c, archive, tag, stages)
		if e != nil {
			return e
		}
		s.ReleaseURL, s.Size, s.SHA256 = link, size, sha
		s.SuccessRepository = c.Repository
		s.LastSuccessAt = time.Now().UTC()
		finishRetain := stages.start("retention")
		e = g.Retain(ctx, c, tag)
		finishRetain()
		s.Message = "备份已上传，下载校验及本地恢复验证通过"
		if e != nil {
			s.Message = "备份已验证，但旧备份清理失败，请检查 GitHub 权限"
		}
		return nil
	}()
	s.Running = false
	s.FinishedAt = time.Now().UTC()
	s.TotalDurationMS = time.Since(started).Milliseconds()
	log.Printf("backup completed success=%t duration_ms=%d", jobErr == nil, s.TotalDurationMS)
	if jobErr != nil {
		s.Message = "备份失败：" + jobErr.Error()
	} else {
		s.Phase = ""
	}
	if err = writeJSON(filepath.Join(dir, "status.json"), s); err != nil {
		return err
	}
	return jobErr
}

func within(file, dir string) bool {
	rel, err := filepath.Rel(dir, file)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

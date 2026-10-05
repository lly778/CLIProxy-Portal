package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const DatabaseRestoreMode = "replace-portal-database"
const RebuildRestoreMode = "rebuild-and-restore"
const transactionName = ".database-restore-transaction"

type PortalRestorePolicy struct {
	Database             string `json:"database"`
	Container            string `json:"container"`
	HealthURL            string `json:"health_url"`
	UpstreamConfig       string `json:"upstream_config"`
	Auths                string `json:"auths"`
	UpstreamContainer    string `json:"upstream_container"`
	UpstreamHealthURL    string `json:"upstream_health_url"`
	ManagerContainer     string `json:"manager_container"`
	ManagerHealthURL     string `json:"manager_health_url"`
	ManagerDatabase      string `json:"manager_database"`
	ManagerDataKey       string `json:"manager_data_key"`
	ManagerVolume        string `json:"manager_volume"`
	ComposeFile          string `json:"compose_file"`
	ComposeSHA256        string `json:"compose_sha256"`
	ProjectName          string `json:"project_name"`
	UpstreamService      string `json:"upstream_service"`
	ManagerService       string `json:"manager_service"`
	UpstreamRepository   string `json:"upstream_repository"`
	ManagerRepository    string `json:"manager_repository"`
	ManagerKeyFile       string `json:"manager_key_file"`
	CPAKeyFile           string `json:"cpa_key_file"`
	PortalManagerKeyFile string `json:"portal_manager_key_file"`
}

type portalController interface {
	Verify(context.Context, PortalRestorePolicy) error
	Stop(context.Context) error
	Start(context.Context) error
	Healthy(context.Context) error
}

type deploymentState struct {
	Images  map[string]string `json:"images,omitempty"`
	Missing bool              `json:"missing,omitempty"`
}
type deploymentController interface {
	OriginalDeployment(context.Context) (deploymentState, error)
	DesiredDeployment() deploymentState
	RestoreDeployment(deploymentState) error
}

type restoreTarget struct {
	Path    string `json:"path"`
	Archive string `json:"archive"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	Exists  bool   `json:"exists"`
}
type databaseTransaction struct {
	Mode              string          `json:"mode"`
	Phase             string          `json:"phase"`
	Rollback          string          `json:"rollback"`
	Targets           []restoreTarget `json:"targets"`
	Deployment        deploymentState `json:"deployment"`
	DesiredDeployment deploymentState `json:"desired_deployment"`
}

var generatedDir = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var containerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}$`)

func LoadRestorePolicy(file string) (PortalRestorePolicy, error) {
	var p PortalRestorePolicy
	if err := trustedHostFile(file); err != nil {
		return p, errors.New("后台恢复策略未配置或权限不安全")
	}
	if err := readJSON(file, &p); err != nil {
		return p, errors.New("后台恢复策略无法读取")
	}
	return p, p.validate(DatabaseRestoreMode)
}

func localEndpoint(endpoint, path string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "http" && u.User == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") && u.Path == path && u.RawQuery == "" && u.Fragment == ""
}

func (p PortalRestorePolicy) targets(mode string) []restoreTarget {
	targets := []restoreTarget{{Path: p.Database, Archive: "portal/data/portal.db", Kind: "database"}}
	if mode == RebuildRestoreMode {
		targets = append(targets,
			restoreTarget{Path: p.UpstreamConfig, Archive: "upstream/cliproxyapi/config.yaml", Kind: "file"},
			restoreTarget{Path: p.Auths, Archive: "upstream/cliproxyapi/auths", Kind: "directory"},
			restoreTarget{Path: p.ComposeFile, Archive: "upstream/compose.yaml", Kind: "file"},
			restoreTarget{Path: filepath.Join(filepath.Dir(p.ComposeFile), ".env"), Archive: "upstream/.env", Kind: "file"},
			restoreTarget{Path: p.ManagerKeyFile, Archive: "upstream/secrets/cpamp-admin-key", Kind: "file"},
			restoreTarget{Path: p.CPAKeyFile, Archive: "upstream/secrets/cpa-management-key", Kind: "file"},
			restoreTarget{Path: p.PortalManagerKeyFile, Archive: "portal/secrets/cpamp_admin_key", Kind: "file"},
			restoreTarget{Path: p.ManagerDatabase, Archive: managerDatabaseName, Kind: "manager-database"},
			restoreTarget{Path: p.ManagerDataKey, Archive: managerDataKeyName, Kind: "file"},
		)
	}
	return targets
}

func (p PortalRestorePolicy) validate(mode string) error {
	if mode != DatabaseRestoreMode && mode != RebuildRestoreMode {
		return errors.New("恢复模式无效")
	}
	if !containerName.MatchString(p.Container) || !localEndpoint(p.HealthURL, "/healthz") {
		return errors.New("门户恢复服务或本机健康检查无效")
	}
	if mode == RebuildRestoreMode {
		for _, name := range []string{p.UpstreamContainer, p.ManagerContainer, p.ProjectName, p.UpstreamService, p.ManagerService, p.ManagerVolume} {
			if !containerName.MatchString(name) {
				return errors.New("后台重建服务配置不完整")
			}
		}
		parent := filepath.Dir(p.ManagerDatabase)
		if filepath.Base(p.ManagerDatabase) != "usage.sqlite" || p.ManagerDataKey != filepath.Join(parent, "data.key") {
			return errors.New("CPAMP 数据库和 data.key 必须位于同一批准的数据卷")
		}
		if p.Container == p.UpstreamContainer || p.Container == p.ManagerContainer || p.UpstreamContainer == p.ManagerContainer || p.UpstreamService == p.ManagerService || !localEndpoint(p.UpstreamHealthURL, "/") || !localEndpoint(p.ManagerHealthURL, "/health") {
			return errors.New("重建服务或本机健康检查无效")
		}
	}
	targets := p.targets(mode)
	for i, target := range targets {
		if !filepath.IsAbs(target.Path) || filepath.Clean(target.Path) != target.Path || filepath.Dir(target.Path) == target.Path {
			return errors.New("恢复目标必须是明确的绝对路径")
		}
		for _, previous := range targets[:i] {
			if target.Path == previous.Path || (target.Kind == "directory" && within(previous.Path, target.Path)) || (previous.Kind == "directory" && within(target.Path, previous.Path)) {
				return errors.New("恢复文件和目录不能重叠")
			}
		}
	}
	return nil
}

func DatabaseRecoveryPending(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, transactionName))
	return !errors.Is(err, os.ErrNotExist)
}

// The host controls every destination and service. Candidate data is fully
// validated and staged on each target filesystem before any service is stopped.
func replacePortalDatabase(ctx context.Context, dir, restored string, p PortalRestorePolicy, service portalController, mode ...string) (rollback string, result error) {
	selected := DatabaseRestoreMode
	if len(mode) > 0 {
		selected = mode[0]
	}
	if err := p.validate(selected); err != nil {
		return "", err
	}
	if DatabaseRecoveryPending(dir) {
		return "", errors.New("存在未完成的恢复事务，请先处理回退")
	}
	if err := service.Verify(ctx, p); err != nil {
		return "", err
	}
	if selected == RebuildRestoreMode {
		if err := validateManagerBackup(ctx, restored, true); err != nil {
			return "", err
		}
	}
	if err := restoreSpace(p.targets(selected), restored, dir); err != nil {
		return "", err
	}
	j := databaseTransaction{Mode: selected, Phase: "preparing", Targets: p.targets(selected)}
	if selected == RebuildRestoreMode {
		deployment, ok := service.(deploymentController)
		if !ok {
			return "", errors.New("后台重建服务不可用")
		}
		var err error
		j.Deployment, err = deployment.OriginalDeployment(ctx)
		if err != nil {
			return "", err
		}
		j.DesiredDeployment = deployment.DesiredDeployment()
	}
	transaction := filepath.Join(dir, transactionName)
	if err := os.Mkdir(transaction, 0o700); err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			cleanupDatabaseStages(j)
			_ = os.RemoveAll(transaction)
		}
	}()
	rollbackDir, err := os.MkdirTemp(dir, "restore-rollback-")
	if err != nil {
		return "", err
	}
	j.Rollback = filepath.Base(rollbackDir)
	defer func() {
		if j.Phase == "preparing" {
			_ = os.RemoveAll(rollbackDir)
		}
	}()
	for index := range j.Targets {
		target := &j.Targets[index]
		parent := filepath.Dir(target.Path)
		actual, err := filepath.EvalSymlinks(parent)
		if err != nil || actual != parent {
			return "", errors.New("恢复目标目录不能通过链接重定向")
		}
		info, err := os.Lstat(target.Path)
		if err == nil {
			target.Exists = true
			if (target.Kind == "directory" && !info.IsDir()) || (target.Kind != "directory" && !info.Mode().IsRegular()) {
				return "", errors.New("恢复目标类型无效，不允许链接")
			}
		} else if !errors.Is(err, os.ErrNotExist) || target.Kind == "database" {
			return "", errors.New("当前门户数据库或恢复目标无法读取")
		}
		stage, err := os.MkdirTemp(parent, ".portal-live-restore-")
		if err != nil {
			return "", err
		}
		target.Stage = filepath.Base(stage)
		candidate := filepath.Join(stage, "candidate")
		source := filepath.Join(restored, filepath.FromSlash(target.Archive))
		if target.Kind == "directory" {
			err = copyAuths(ctx, source, candidate)
		} else {
			err = copyRegular(ctx, source, candidate)
		}
		if err != nil {
			return "", err
		}
		if target.Kind == "database" {
			if err = preparePortalDatabase(ctx, candidate); err != nil {
				return "", err
			}
		}
		if target.Kind == "manager-database" {
			if err = checkManagerDatabase(ctx, candidate); err != nil {
				return "", err
			}
		}
		if err = prepareTarget(*target, candidate); err != nil {
			return "", err
		}
	}
	if err = persistDatabaseTransaction(transaction, j); err != nil {
		return "", err
	}
	j.Phase = "stopping"
	if err = persistDatabaseTransaction(transaction, j); err != nil {
		return "", err
	}
	err = service.Stop(ctx)
	for index, target := range j.Targets {
		if err != nil {
			break
		}
		if !target.Exists {
			continue
		}
		snapshot := filepath.Join(rollbackDir, fmt.Sprint(index))
		switch target.Kind {
		case "database", "manager-database":
			err = snapshotSQLite(ctx, target.Path, snapshot)
		case "directory":
			err = copyAuths(ctx, target.Path, snapshot)
		default:
			err = copyRegular(ctx, target.Path, snapshot)
		}
		if err == nil && target.Kind != "directory" {
			err = syncFile(snapshot)
		}
	}
	if err == nil {
		err = syncDirectory(rollbackDir)
	}
	if err == nil {
		j.Phase = "switching"
		err = persistDatabaseTransaction(transaction, j)
	}
	for _, target := range j.Targets {
		if err != nil {
			break
		}
		stage := filepath.Join(filepath.Dir(target.Path), target.Stage)
		if isDatabaseTarget(target) {
			err = moveDatabaseSidecars(target.Path, rollbackDir)
		}
		if err == nil && target.Kind == "directory" && target.Exists {
			err = os.Rename(target.Path, filepath.Join(stage, "previous"))
		}
		if err == nil {
			err = os.Rename(filepath.Join(stage, "candidate"), target.Path)
		}
		if err == nil {
			err = syncDirectory(filepath.Dir(target.Path))
		}
	}
	if err == nil {
		j.Phase = "starting"
		err = persistDatabaseTransaction(transaction, j)
	}
	if err == nil {
		err = service.Start(ctx)
	}
	if err == nil {
		err = service.Healthy(ctx)
	}
	if err == nil {
		committed := j
		committed.Phase = "committed"
		err = persistDatabaseTransaction(transaction, committed)
		if err == nil {
			j = committed
			return j.Rollback, nil
		}
	}
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if rollbackErr := rollbackDatabaseTransaction(rollbackCtx, dir, p, service, &j); rollbackErr != nil {
		keep = true
		return j.Rollback, fmt.Errorf("恢复失败，自动回退未完成；事务和回退副本已保留，请检查后台任务: %w", rollbackErr)
	}
	return j.Rollback, fmt.Errorf("恢复失败，已回退原数据及服务: %w", err)
}

func prepareTarget(target restoreTarget, staged string) error {
	owner := filepath.Dir(target.Path)
	if target.Exists && target.Kind != "database" {
		owner = target.Path
	}
	apply := func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		if err = inheritOwner(owner, f); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if entry != nil && entry.IsDir() {
			mode = 0o700
		}
		if err = f.Chmod(mode); err != nil {
			return err
		}
		if entry != nil && entry.IsDir() {
			return syncDirectory(file)
		}
		return syncFile(file)
	}
	if target.Kind == "directory" {
		return filepath.WalkDir(staged, apply)
	}
	if err := apply(staged, nil, nil); err != nil {
		return err
	}
	return nil
}

func rollbackDatabaseTransaction(ctx context.Context, dir string, p PortalRestorePolicy, service portalController, j *databaseTransaction) error {
	if err := trustedHostDirectory(filepath.Join(dir, j.Rollback)); err != nil {
		return err
	}
	if j.Mode == RebuildRestoreMode {
		deployment, ok := service.(deploymentController)
		if !ok {
			return errors.New("后台重建回退服务不可用")
		}
		if err := deployment.RestoreDeployment(j.Deployment); err != nil {
			return err
		}
	}
	if j.Phase == "switching" || j.Phase == "starting" {
		if err := service.Stop(ctx); err != nil {
			return err
		}
		for index, target := range j.Targets {
			stage := filepath.Join(filepath.Dir(target.Path), target.Stage)
			if err := trustedHostDirectory(stage); err != nil {
				return err
			}
			attempt, err := os.MkdirTemp(stage, "rollback-")
			if err != nil {
				return err
			}
			snapshot := filepath.Join(dir, j.Rollback, fmt.Sprint(index))
			candidate := filepath.Join(attempt, "candidate")
			if target.Exists {
				switch target.Kind {
				case "directory":
					err = copyAuths(ctx, snapshot, candidate)
				default:
					err = copyRegular(ctx, snapshot, candidate)
				}
				if err != nil {
					return err
				}
				if isDatabaseTarget(target) {
					if err = checkSQLite(ctx, candidate); err != nil {
						return err
					}
				}
				// A directory can be absent if replacement was interrupted between moves.
				ownerTarget := target
				if _, err = os.Lstat(target.Path); errors.Is(err, os.ErrNotExist) {
					ownerTarget.Exists = false
				}
				if err = prepareTarget(ownerTarget, candidate); err != nil {
					return err
				}
			}
			if isDatabaseTarget(target) {
				if err = moveDatabaseSidecars(target.Path, filepath.Join(dir, j.Rollback)); err != nil {
					return err
				}
			}
			if info, err := os.Lstat(target.Path); err == nil {
				if (target.Kind == "directory" && !info.IsDir()) || (target.Kind != "directory" && !info.Mode().IsRegular()) {
					return errors.New("回退目标类型无效")
				}
				if err = os.Rename(target.Path, filepath.Join(attempt, "displaced")); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if target.Exists {
				if err = os.Rename(candidate, target.Path); err != nil {
					return err
				}
			}
			if err = syncDirectory(filepath.Dir(target.Path)); err != nil {
				return err
			}
		}
		j.Phase = "rolled-back"
		if err := persistDatabaseTransaction(filepath.Join(dir, transactionName), *j); err != nil {
			return err
		}
	}
	if err := service.Start(ctx); err != nil {
		return err
	}
	return service.Healthy(ctx)
}

func recoverDatabaseTransaction(ctx context.Context, dir string, p PortalRestorePolicy, service portalController) error {
	transaction := filepath.Join(dir, transactionName)
	if err := trustedHostDirectory(transaction); err != nil {
		return err
	}
	var j databaseTransaction
	if err := readJSON(filepath.Join(transaction, "transaction.json"), &j); err != nil {
		return errors.New("恢复事务记录无法读取，请管理员检查")
	}
	if err := p.validate(j.Mode); err != nil {
		return err
	}
	targets := p.targets(j.Mode)
	if len(targets) != len(j.Targets) || !generatedDir.MatchString(j.Rollback) || !strings.HasPrefix(j.Rollback, "restore-rollback-") {
		return errors.New("恢复事务目标无效")
	}
	if err := trustedHostDirectory(filepath.Join(dir, j.Rollback)); err != nil {
		return err
	}
	for index, target := range j.Targets {
		if target.Path != targets[index].Path || target.Archive != targets[index].Archive || target.Kind != targets[index].Kind || !generatedDir.MatchString(target.Stage) || !strings.HasPrefix(target.Stage, ".portal-live-restore-") {
			return errors.New("恢复事务与后台策略不一致")
		}
		if err := trustedHostDirectory(filepath.Join(filepath.Dir(target.Path), target.Stage)); err != nil {
			return err
		}
	}
	if err := service.Verify(ctx, p); err != nil {
		return err
	}
	message := "上次恢复异常中断，已回退原数据并启动服务"
	switch j.Phase {
	case "committed":
		if j.Mode == RebuildRestoreMode {
			deployment, ok := service.(deploymentController)
			if !ok {
				return errors.New("后台重建任务不可用")
			}
			if err := deployment.RestoreDeployment(j.DesiredDeployment); err != nil {
				return err
			}
		}
		if err := service.Start(ctx); err != nil {
			return err
		}
		if err := service.Healthy(ctx); err != nil {
			return err
		}
		message = "恢复已完成，服务健康；请重新登录"
	case "preparing", "stopping", "switching", "starting", "rolled-back":
		if err := rollbackDatabaseTransaction(ctx, dir, p, service, &j); err != nil {
			return err
		}
	default:
		return errors.New("恢复事务阶段无效")
	}
	if err := writeJSON(filepath.Join(dir, "restore-status.json"), RestoreStatus{FinishedAt: time.Now().UTC(), Rollback: j.Rollback, Message: message}); err != nil {
		return err
	}
	cleanupDatabaseStages(j)
	return os.RemoveAll(transaction)
}

func cleanupDatabaseStages(j databaseTransaction) {
	for _, target := range j.Targets {
		if generatedDir.MatchString(target.Stage) && strings.HasPrefix(target.Stage, ".portal-live-restore-") {
			_ = os.RemoveAll(filepath.Join(filepath.Dir(target.Path), target.Stage))
		}
	}
}

func persistDatabaseTransaction(dir string, j databaseTransaction) error {
	if err := writeJSON(filepath.Join(dir, "transaction.json"), j); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncFile(file string) error {
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func moveDatabaseSidecars(database, rollbackDir string) error {
	attempt, err := os.MkdirTemp(rollbackDir, "sidecars-")
	if err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		file := database + suffix
		info, err := os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("数据库附属文件无效")
		}
		if err = os.Rename(file, filepath.Join(attempt, "portal.db"+suffix)); err != nil {
			return err
		}
	}
	return syncDirectory(attempt)
}

func preparePortalDatabase(ctx context.Context, file string) error {
	if err := checkSQLite(ctx, file); err != nil {
		return err
	}
	path := filepath.ToSlash(file)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}).String())
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, table := range []string{"users", "api_keys", "sessions", "password_resets", "sync_jobs", "settings", "audit_logs"} {
		var n int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 1 {
			return errors.New("备份不是完整的门户数据库")
		}
	}
	var admins int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role='admin' AND status='approved' AND deleted_at_ms=0 AND password_hash<>''").Scan(&admins); err != nil || admins == 0 {
		return errors.New("备份缺少可登录的管理员，已拒绝覆盖")
	}
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Never replay old upstream mutations or revive old login/reset tokens.
	for _, table := range []string{"sessions", "password_resets", "sync_jobs"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isDatabaseTarget(target restoreTarget) bool {
	return target.Kind == "database" || target.Kind == "manager-database"
}

func copyAuths(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() {
		return errors.New("备份缺少有效的 auths 目录")
	}
	if err = os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	var size int64
	count := 0
	err = filepath.WalkDir(source, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, file)
		if err != nil || !within(file, source) {
			return errors.New("auths 路径无效")
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		stat, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if stat.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		if !stat.Mode().IsRegular() {
			return errors.New("auths 不允许链接或特殊文件")
		}
		size += stat.Size()
		count++
		if size > maxArchiveSize || count > 10000 {
			return errors.New("auths 超过恢复限制")
		}
		if err = copyRegular(ctx, file, target); err != nil {
			return err
		}
		return syncFile(target)
	})
	if err != nil {
		return err
	}
	return syncDirectory(destination)
}

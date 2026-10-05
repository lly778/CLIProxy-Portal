package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func checkManagerDatabase(ctx context.Context, file string) error {
	if err := checkSQLite(ctx, file); err != nil {
		return err
	}
	db, err := openSQLite(file)
	if err != nil {
		return err
	}
	defer db.Close()
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='usage_events'").Scan(&count); err != nil || count != 1 {
		return errors.New("备份不是 CPAMP 用量数据库")
	}
	return nil
}

func validateManagerBackup(ctx context.Context, dir string, required bool) error {
	db := filepath.Join(dir, filepath.FromSlash(managerDatabaseName))
	key := filepath.Join(dir, filepath.FromSlash(managerDataKeyName))
	_, dbErr := os.Lstat(db)
	_, keyErr := os.Lstat(key)
	if !required && os.IsNotExist(dbErr) && os.IsNotExist(keyErr) {
		return nil // Older packages can still restore only the portal database.
	}
	if dbErr != nil || keyErr != nil {
		return errors.New("备份缺少成套 CPAMP 数据库和 data.key；旧备份可使用普通门户恢复")
	}
	info, err := os.Lstat(key)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 4096 {
		return errors.New("CPAMP data.key 缺失或无效")
	}
	if err = checkManagerDatabase(ctx, db); err != nil {
		return err
	}
	if required {
		return managerArchiveRestoreSafe(ctx, db)
	}
	return nil
}

// Archive files are deliberately out of scope. Keep the database snapshot
// unmodified (including its identity ledger), but never resume a maintenance
// run or pretend that a partial archive history is a complete rebuild.
func managerArchiveRestoreSafe(ctx context.Context, file string) error {
	db, err := openSQLite(file)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, table := range []string{"usage_archive_runs", "usage_archive_segments"} {
		var present int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			continue
		}
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("CPAMP 数据库含归档任务，但本备份不包含 usage-archives；为防止不一致，拒绝自动重建恢复，请管理员处理配套归档。普通门户恢复不受影响")
		}
	}
	return nil
}

func managerSources(sources []Source, p PortalRestorePolicy) (bool, error) {
	paths := map[string]string{managerDatabaseName: p.ManagerDatabase, managerDataKeyName: p.ManagerDataKey}
	present := false
	for _, source := range sources {
		if expected, ok := paths[source.Name]; ok {
			present = true
			if source.Path != expected || expected == "" {
				return true, errors.New("CPAMP 备份来源与后台批准的数据卷不一致")
			}
		}
	}
	return present, validateSources(sources)
}

func dataSize(file string) (int64, error) {
	var total int64
	count := 0
	err := filepath.WalkDir(file, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) && path == file {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("灾备数据不允许链接或特殊文件")
		}
		total += info.Size()
		count++
		if total > maxArchiveSize || count > 10000 {
			return errors.New("灾备数据超过容量或文件数限制")
		}
		return nil
	})
	return total, err
}

func backupSpace(sources []Source, dir string) error {
	var size int64
	for _, source := range sources {
		n, err := dataSize(source.Path)
		if err != nil {
			return err
		}
		size += n
		if source.SQLite {
			n, err = dataSize(source.Path + "-wal")
			if err != nil {
				return err
			}
			size += n
		}
	}
	// Peak staging, compressed input and local restore verification; compression
	// streams to encryption and snapshots are removed before verification.
	return requireSpace(dir, 3*size+(128<<20))
}

func restoreSpace(targets []restoreTarget, restored, state string) error {
	var oldSize, newSize int64
	for _, target := range targets {
		n, err := dataSize(filepath.Join(restored, filepath.FromSlash(target.Archive)))
		if err != nil {
			return err
		}
		newSize += n
		n, err = dataSize(target.Path)
		if err != nil {
			return err
		}
		oldSize += n
		if isDatabaseTarget(target) {
			n, err = dataSize(target.Path + "-wal")
			if err != nil {
				return err
			}
			oldSize += n
		}
	}
	// Reserve candidates, rollback snapshots and an additional rollback attempt
	// on EVERY target filesystem before stopping any service.
	for _, target := range targets {
		if err := requireSpace(filepath.Dir(target.Path), newSize+2*oldSize+(128<<20)); err != nil {
			return err
		}
	}
	return requireSpace(state, newSize+2*oldSize+(128<<20))
}

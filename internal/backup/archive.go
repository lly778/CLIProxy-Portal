package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const maxArchiveSize int64 = 8 << 30

const managerDatabaseName = "upstream/cpamp/usage.sqlite"
const managerDataKeyName = "upstream/cpamp/data.key"

type Source struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	SQLite    bool   `json:"sqlite,omitempty"`
	Directory bool   `json:"directory,omitempty"`
	Required  bool   `json:"required"`
}

type Entry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	SQLite bool   `json:"sqlite,omitempty"`
}

type Manifest struct {
	Version     int               `json:"version"`
	Kind        string            `json:"kind"`
	CreatedAt   time.Time         `json:"created_at"`
	Entries     []Entry           `json:"entries"`
	Directories []string          `json:"directories,omitempty"`
	Images      map[string]string `json:"images,omitempty"`
}

func validateSources(sources []Source) error {
	allowed := map[string]bool{"portal/data/portal.db": true, "upstream/cliproxyapi/config.yaml": true, "upstream/cliproxyapi/auths": true, "portal/secrets/portal_app_secret": true, "portal/secrets/cpamp_admin_key": true, "portal/.env": true, "portal/compose.prebuilt.yaml": true, "portal/compose.gateway.yaml": true, "upstream/compose.yaml": true, "upstream/.env": true, "upstream/secrets/cpamp-admin-key": true, "upstream/secrets/cpa-management-key": true, "upstream/versions.json": true}
	seen := map[string]bool{}
	allowed[managerDatabaseName], allowed[managerDataKeyName] = true, true
	for _, source := range sources {
		if seen[source.Name] || !allowed[source.Name] {
			return errors.New("备份来源不可重复或超出灾备范围")
		}
		seen[source.Name] = true
		switch source.Name {
		case "portal/data/portal.db", managerDatabaseName:
			if !source.SQLite || source.Directory {
				return errors.New("数据库必须采用一致性快照")
			}
		case "upstream/cliproxyapi/config.yaml":
			if source.SQLite || source.Directory {
				return errors.New("config.yaml 必须是普通文件")
			}
		case "upstream/cliproxyapi/auths":
			if !source.Directory || source.SQLite {
				return errors.New("auths 必须是普通目录")
			}
		default:
			if source.SQLite || source.Directory {
				return errors.New("部署参数及密钥必须是普通文件")
			}
		}
	}
	if seen[managerDatabaseName] || seen[managerDataKeyName] {
		if !seen[managerDatabaseName] || !seen[managerDataKeyName] {
			return errors.New("CPAMP 数据库和 data.key 来源必须成套配置")
		}
		for _, source := range sources {
			if (source.Name == managerDatabaseName || source.Name == managerDataKeyName) && !source.Required {
				return errors.New("CPAMP 数据库和 data.key 不能设为可选")
			}
		}
	}
	if !seen["portal/data/portal.db"] || !seen["upstream/cliproxyapi/config.yaml"] || !seen["upstream/cliproxyapi/auths"] {
		return errors.New("备份缺少数据库、config.yaml 或 auths 来源")
	}
	return nil
}

func LoadSources(file string) ([]Source, error) {
	var sources []Source
	if err := readJSON(file, &sources); err != nil {
		return nil, errors.New("无法读取宿主机备份来源配置")
	}
	if len(sources) == 0 || len(sources) > 100 {
		return nil, errors.New("备份来源数量无效")
	}
	return sources, nil
}

func safeName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\:\x00\r\n") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../") && (strings.HasPrefix(name, "portal/") || strings.HasPrefix(name, "upstream/"))
}

// Create first snapshots databases and copies regular configuration files into
// a private temporary directory. No live file is archived by an unsafe raw copy.
func Create(ctx context.Context, sources []Source, dir string, key []byte, images ...map[string]string) (string, Manifest, error) {
	var versions map[string]string
	if len(images) > 0 {
		versions = images[0]
	}
	return createArchive(ctx, sources, dir, key, versions, nil)
}

// freeze covers only snapshot collection. CPAMP resumes before compression,
// encryption, local restore verification and network upload.
func createArchive(ctx context.Context, sources []Source, dir string, key []byte, images map[string]string, freeze func(func() error) error, observers ...*stageHooks) (string, Manifest, error) {
	var stages *stageHooks
	if len(observers) > 0 {
		stages = observers[0]
	}
	finishSnapshot := stages.start("snapshot")
	defer finishSnapshot()
	m := Manifest{Version: 1, Kind: manifestKind, CreatedAt: time.Now().UTC()}
	m.Images = images
	if err := validateSources(sources); err != nil {
		return "", m, err
	}
	m.Directories = []string{"upstream/cliproxyapi/auths"}
	stage := filepath.Join(dir, "files")
	if err := os.Mkdir(stage, 0o700); err != nil {
		return "", m, err
	}
	defer os.RemoveAll(stage) // This invocation's snapshot directory only.
	seen := map[string]bool{}
	var total int64
	add := func(src, name string, sqlite bool) error {
		if len(m.Entries) >= 10000 || filepath.Base(src) == "recovery.key" || filepath.Base(src) == "github.token" {
			return errors.New("备份来源数量过多或包含不允许打包的密钥")
		}
		if !safeName(name) || seen[name] {
			return errors.New("备份文件名无效或重复")
		}
		seen[name] = true
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("备份来源必须为普通文件，不能是符号链接")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(stage, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if sqlite {
			err = snapshotSQLite(ctx, src, dst)
		} else {
			err = copyRegular(ctx, src, dst)
		}
		if err != nil {
			return fmt.Errorf("生成 %s 快照失败: %w", name, err)
		}
		entry, err := describe(dst, name, sqlite)
		if err != nil {
			return err
		}
		total += entry.Size
		if total > maxArchiveSize {
			return errors.New("备份解压体积超过 8 GiB 限制")
		}
		m.Entries = append(m.Entries, entry)
		return nil
	}
	collect := func() error {
		for _, source := range sources {
			if source.Name == "portal/data/portal.db" && !source.SQLite {
				return errors.New("门户数据库必须使用 SQLite 一致性快照")
			}
			if !filepath.IsAbs(source.Path) || !safeName(source.Name) || source.SQLite && source.Directory {
				return errors.New("宿主机备份来源配置无效")
			}
			info, err := os.Lstat(source.Path)
			if errors.Is(err, os.ErrNotExist) && !source.Required {
				continue
			}
			if err != nil {
				return fmt.Errorf("备份来源 %s 不可用", source.Name)
			}
			if source.Directory {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return errors.New("备份目录不能是符号链接")
				}
				err = filepath.WalkDir(source.Path, func(p string, d os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if d.Type()&os.ModeSymlink != 0 {
						return errors.New("备份目录中存在符号链接")
					}
					if d.IsDir() {
						return nil
					}
					rel, e := filepath.Rel(source.Path, p)
					if e != nil {
						return e
					}
					return add(p, source.Name+"/"+filepath.ToSlash(rel), false)
				})
			} else {
				err = add(source.Path, source.Name, source.SQLite)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if freeze != nil {
		err = freeze(collect)
	} else {
		err = collect()
	}
	if err != nil {
		return "", m, err
	}
	// Never call a partial configuration-only package a recoverable portal backup.
	if !seen["portal/data/portal.db"] || !seen["upstream/cliproxyapi/config.yaml"] {
		return "", m, errors.New("备份缺少门户数据库或 CPA 配置")
	}
	if seen[managerDatabaseName] {
		// snapshotSQLite already checked these exact immutable staged bytes.
		if err = validateManagerBackupIntegrity(ctx, stage, false, false); err != nil {
			return "", m, err
		}
	}
	finishSnapshot()
	finishCompression := stages.start("compress_encrypt")
	defer finishCompression()
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Name < m.Entries[j].Name })
	encrypted := filepath.Join(dir, archiveName)
	out, err := os.OpenFile(encrypted, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", m, err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		e := writeTarGzip(ctx, writer, stage, m)
		_ = writer.CloseWithError(e)
		done <- e
	}()
	err = Encrypt(&limitedWriter{writer: out, remaining: MaxEncryptedSize}, reader, key)
	_ = reader.CloseWithError(err)
	if e := <-done; err == nil {
		err = e
	}
	if err == nil {
		err = out.Sync()
	}
	if e := out.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = os.Remove(encrypted)
	}
	return encrypted, m, err
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("加密备份超过 GitHub 单附件 2 GiB 限制")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func writeTarGzip(ctx context.Context, output io.Writer, stage string, m Manifest) error {
	// Keep the gzip/container format compatible, but favor low CPU consumption.
	gz, err := gzip.NewWriterLevel(output, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, entry := range m.Entries {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = tw.WriteHeader(&tar.Header{Name: entry.Name, Mode: 0o600, Size: entry.Size, Typeflag: tar.TypeReg}); err != nil {
			break
		}
		var source *os.File
		source, err = os.Open(filepath.Join(stage, filepath.FromSlash(entry.Name)))
		if err != nil {
			break
		}
		_, err = io.Copy(tw, source)
		_ = source.Close()
		if err != nil {
			break
		}
	}
	if err == nil {
		var b []byte
		b, err = json.Marshal(m)
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = tw.Write(b)
		}
	}
	for _, closeFn := range []func() error{tw.Close, gz.Close} {
		if e := closeFn(); err == nil {
			err = e
		}
	}
	return err
}

func openSQLite(file string) (*sql.DB, error) {
	filePath := filepath.ToSlash(file)
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	u := &url.URL{Scheme: "file", Path: filePath}
	u.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func snapshotSQLite(ctx context.Context, src, dst string) error {
	db, err := openSQLite(src)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	// VACUUM INTO is a consistent compact snapshot, including committed WAL data.
	if _, err = db.ExecContext(ctx, "VACUUM main INTO ?", dst); err != nil {
		return errors.New("SQLite 一致性快照失败")
	}
	if err = os.Chmod(dst, 0o600); err != nil {
		return err
	}
	return checkSQLite(ctx, dst)
}

func checkSQLite(ctx context.Context, file string) error {
	db, err := openSQLite(file)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return errors.New("数据库备份完整性检查失败")
	}
	return nil
}

func copyRegular(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxArchiveSize {
		return errors.New("备份来源过大或不是普通文件")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err = ctx.Err(); err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxArchiveSize+1))
	if err != nil {
		return err
	}
	after, err := in.Stat()
	if err != nil || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("配置文件在备份期间发生变化，请重试")
	}
	return nil
}

func describe(file, name string, sqlite bool) (Entry, error) {
	f, err := os.Open(file)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return Entry{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), SQLite: sqlite}, err
}

// Restore always stages into a new directory. It never overwrites a live
// database, follows archive links, executes package content or starts services.
func Restore(ctx context.Context, file, keyFile, output string) error {
	output, err := filepath.Abs(output)
	if err != nil || filepath.Dir(output) == output {
		return errors.New("恢复目标无效")
	}
	if _, err = os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return errors.New("恢复目标已存在；请指定全新的目录，禁止覆盖现有服务")
	}
	key, err := ReadKey(keyFile)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(output), ".portal-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work) // Only this invocation's private generated directory.
	input, err := os.Open(file)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxEncryptedSize {
		return errors.New("加密备份文件类型或大小无效")
	}
	if err = requireSpace(work, 128<<20); err != nil {
		return err
	}
	decrypted := newDecryptedStream(ctx, io.LimitReader(input, MaxEncryptedSize+1), key)
	defer decrypted.Close()
	gz, err := gzip.NewReader(decrypted)
	if err != nil {
		return err
	}
	defer gz.Close()
	dir := filepath.Join(work, "restored")
	if err = os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	entries := map[string]Entry{}
	var manifest Manifest
	manifestSeen := false
	var total int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maxArchiveSize {
			return errors.New("备份包含不允许的文件类型或大小")
		}
		if header.Name == "manifest.json" {
			if manifestSeen || header.Size > 1<<20 {
				return errors.New("备份清单无效")
			}
			manifestSeen = true
			if err = json.NewDecoder(tr).Decode(&manifest); err != nil {
				return err
			}
			continue
		}
		if !safeName(header.Name) {
			return errors.New("备份包含不安全路径")
		}
		if _, exists := entries[header.Name]; exists {
			return errors.New("备份包含重复文件")
		}
		total += header.Size
		if total > maxArchiveSize || len(entries) >= 10000 {
			return errors.New("备份解压体积过大")
		}
		if err = requireSpace(work, header.Size+(128<<20)); err != nil {
			return err
		}
		hash := sha256.New()
		dst := filepath.Join(dir, filepath.FromSlash(header.Name))
		if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(io.MultiWriter(out, hash), tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != header.Size {
			return errors.New("备份文件不完整")
		}
		entries[header.Name] = Entry{Name: header.Name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	}
	// Drain gzip to validate its checksum, including data after tar EOF.
	var trailing int64
	if trailing, err = io.Copy(io.Discard, io.LimitReader(gz, (1<<20)+1)); err != nil {
		return err
	}
	if trailing > 1<<20 {
		return errors.New("备份包含过多额外压缩内容")
	}
	// Gzip/tar EOF alone is insufficient: authenticate the terminal AES frame
	// and reject appended ciphertext before trusting or promoting any output.
	if err = decrypted.finish(); err != nil {
		return err
	}
	if !manifestSeen || manifest.Version != 1 || !supportedManifestKind(manifest.Kind) || len(manifest.Entries) != len(entries) || len(entries) == 0 {
		return errors.New("备份清单版本或文件数量无效")
	}
	checked := map[string]bool{}
	for _, expected := range manifest.Entries {
		if (expected.Name == "portal/data/portal.db" || expected.Name == managerDatabaseName) && !expected.SQLite {
			return errors.New("门户数据库快照标记无效")
		}
		actual, ok := entries[expected.Name]
		if !ok || checked[expected.Name] || expected.Size != actual.Size || expected.SHA256 != actual.SHA256 {
			return errors.New("备份文件校验不一致")
		}
		checked[expected.Name] = true
		if expected.SQLite {
			if err = checkSQLite(ctx, filepath.Join(dir, filepath.FromSlash(expected.Name))); err != nil {
				return err
			}
		}
	}
	if !checked["portal/data/portal.db"] || !checked["upstream/cliproxyapi/config.yaml"] {
		return errors.New("备份缺少核心恢复文件")
	}
	for _, directory := range manifest.Directories {
		if directory != "upstream/cliproxyapi/auths" {
			return errors.New("备份目录清单无效")
		}
		if err = os.MkdirAll(filepath.Join(dir, filepath.FromSlash(directory)), 0o700); err != nil {
			return err
		}
	}
	// Manifest verification above already quick_checked the restored database.
	if err = validateManagerBackupIntegrity(ctx, dir, false, false); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(dir, "upstream/cliproxyapi/auths")); err != nil || !info.IsDir() {
		return errors.New("备份缺少 auths 目录")
	}
	if err = writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return err
	}
	return os.Rename(dir, output)
}

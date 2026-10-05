package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStreamingEncryptionIntegrity(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	for _, size := range []int{0, 1, chunkSize, chunkSize + 1, chunkSize*3 + 99} {
		plain := bytes.Repeat([]byte{123}, size)
		var encrypted, decoded bytes.Buffer
		if err := Encrypt(&encrypted, bytes.NewReader(plain), key); err != nil {
			t.Fatal(err)
		}
		if err := Decrypt(&decoded, bytes.NewReader(encrypted.Bytes()), key); err != nil || !bytes.Equal(decoded.Bytes(), plain) {
			t.Fatalf("size=%d err=%v", size, err)
		}
		for _, broken := range [][]byte{encrypted.Bytes()[:len(encrypted.Bytes())-1], append(append([]byte{}, encrypted.Bytes()...), 1)} {
			if err := Decrypt(io.Discard, bytes.NewReader(broken), key); err == nil {
				t.Fatal("truncated or extended backup accepted")
			}
		}
		corrupt := append([]byte{}, encrypted.Bytes()...)
		corrupt[len(corrupt)-1] ^= 1
		if err := Decrypt(io.Discard, bytes.NewReader(corrupt), key); err == nil {
			t.Fatal("corrupt backup accepted")
		}
		if err := Decrypt(io.Discard, bytes.NewReader(encrypted.Bytes()), bytes.Repeat([]byte{3}, 32)); err == nil {
			t.Fatal("wrong key accepted")
		}
	}
	var one, two bytes.Buffer
	_ = Encrypt(&one, strings.NewReader("same"), key)
	_ = Encrypt(&two, strings.NewReader("same"), key)
	if bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("file nonce must be random")
	}
}

func fixture(t *testing.T) ([]Source, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE users(name TEXT)", "INSERT INTO users VALUES('live-WAL-user')"} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(dbPath + "-wal"); err != nil {
		t.Fatal("test must include live WAL")
	}
	app := filepath.Join(dir, "app.key")
	config := filepath.Join(dir, "config.yaml")
	auths := filepath.Join(dir, "auths")
	if err = os.Mkdir(auths, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{app: "original-application-secret", config: "api-keys:\n  - original-client-key\n", filepath.Join(auths, "account.json"): "{\"refresh_token\":\"original-upstream-token\"}"} {
		if err = os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []Source{{Path: dbPath, Name: "portal/data/portal.db", SQLite: true, Required: true}, {Path: config, Name: "upstream/cliproxyapi/config.yaml", Required: true}, {Path: auths, Name: "upstream/cliproxyapi/auths", Directory: true, Required: true}}, dir
}

func TestSnapshotArchiveRestoreIncludesWALAndOriginalSecrets(t *testing.T) {
	sources, dir := fixture(t)
	work := filepath.Join(dir, "work")
	_ = os.Mkdir(work, 0o700)
	key := bytes.Repeat([]byte{4}, 32)
	keyFile := filepath.Join(dir, "recovery.key")
	_ = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600)
	file, manifest, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != manifestKind || filepath.Base(file) != archiveName || len(manifest.Entries) != 3 {
		t.Fatalf("manifest=%+v", manifest)
	}
	b, _ := os.ReadFile(file)
	for _, secret := range []string{"original-client-key", "original-upstream-token", "original-application-secret", "live-WAL-user"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("plaintext appeared in encrypted package")
		}
	}
	restored := filepath.Join(dir, "restored")
	if err = Restore(t.Context(), file, keyFile, restored); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(filepath.Join(restored, "portal/data/portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err = db.QueryRow("SELECT name FROM users").Scan(&name); err != nil || name != "live-WAL-user" {
		t.Fatalf("WAL data missing name=%s err=%v", name, err)
	}
	for _, source := range sources[1:2] {
		actual, _ := os.ReadFile(filepath.Join(restored, filepath.FromSlash(source.Name)))
		original, _ := os.ReadFile(source.Path)
		if !bytes.Equal(actual, original) {
			t.Fatal("original config/key not preserved")
		}
	}
	if _, err = os.Stat(filepath.Join(restored, "portal/data/portal.db-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("snapshot should not need WAL")
	}
	// Even valid encryption cannot substitute for checking the file manifest.
	var compressed bytes.Buffer
	if err = Decrypt(&compressed, bytes.NewReader(b), key); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	_ = gz.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(manifest.Entries[0].SHA256), []byte(strings.Repeat("0", 64)), 1)
	var repacked, reencrypted bytes.Buffer
	w := gzip.NewWriter(&repacked)
	_, _ = w.Write(raw)
	_ = w.Close()
	if err = Encrypt(&reencrypted, &repacked, key); err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(dir, "tampered.cbackup")
	_ = os.WriteFile(tampered, reencrypted.Bytes(), 0o600)
	if err = Restore(t.Context(), tampered, keyFile, filepath.Join(dir, "tampered-output")); err == nil {
		t.Fatal("bad manifest checksum accepted")
	}
	if err = Restore(t.Context(), file, keyFile, restored); err == nil {
		t.Fatal("restore overwrote existing directory")
	}
	if err = Restore(t.Context(), file, keyFile, filepath.Dir(filepath.VolumeName(dir)+string(os.PathSeparator))); err == nil {
		t.Fatal("root restore accepted")
	}
	badKey := filepath.Join(dir, "wrong.key")
	_ = os.WriteFile(badKey, []byte(strings.Repeat("01", 32)), 0o600)
	badOutput := filepath.Join(dir, "bad-output")
	if err = Restore(t.Context(), file, badKey, badOutput); err == nil {
		t.Fatal("wrong key restore accepted")
	}
	if _, err = os.Stat(badOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed restore left published output")
	}
}

func TestRestoreSupportsOldNamingButRejectsUnknownManifestKinds(t *testing.T) {
	sources, dir := fixture(t)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{4}, 32)
	keyFile := filepath.Join(dir, "recovery.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	file, _, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{legacyManifestKind, "unknown"} {
		t.Run(kind, func(t *testing.T) {
			var decrypted bytes.Buffer
			if err := Decrypt(&decrypted, bytes.NewReader(encrypted), key); err != nil {
				t.Fatal(err)
			}
			gz, err := gzip.NewReader(&decrypted)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			var compressed, renamed bytes.Buffer
			zw := gzip.NewWriter(&compressed)
			tw := tar.NewWriter(zw)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				if h.Name == "manifest.json" {
					var m Manifest
					if err := json.Unmarshal(body, &m); err != nil {
						t.Fatal(err)
					}
					m.Kind = kind
					body, err = json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					h.Size = int64(len(body))
				}
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(body); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := Encrypt(&renamed, &compressed, key); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), legacyArchiveName)
			if err := os.WriteFile(path, renamed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "restored")
			err = Restore(t.Context(), path, keyFile, out)
			if kind == legacyManifestKind {
				if err != nil {
					t.Fatalf("old backup must remain restorable: %v", err)
				}
			} else if err == nil {
				t.Fatal("unknown manifest kind accepted")
			} else if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid manifest published restored files")
			}
		})
	}
}

func TestSourceValidationAndArchiveTraversal(t *testing.T) {
	sources, dir := fixture(t)
	key := bytes.Repeat([]byte{2}, 32)
	for _, mutate := range []func([]Source) []Source{
		func(s []Source) []Source { return s[:1] },
		func(s []Source) []Source { s[1].Path = filepath.Join(dir, "missing"); return s },
		func(s []Source) []Source { s[1].Name = "portal/../../escape"; return s },
		func(s []Source) []Source { return append(s, s[1]) },
	} {
		work := t.TempDir()
		_, _, err := Create(t.Context(), mutate(append([]Source{}, sources...)), work, key)
		if err == nil {
			t.Fatal("invalid source accepted")
		}
	}
	keyFile := filepath.Join(dir, "key")
	_ = os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600)
	for _, header := range []*tar.Header{
		{Name: "../outside", Size: 1, Typeflag: tar.TypeReg},
		{Name: "portal/../../outside", Size: 1, Typeflag: tar.TypeReg},
		{Name: "portal/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink},
		{Name: "upstream/hard", Linkname: "portal/data/portal.db", Typeflag: tar.TypeLink},
	} {
		var compressed, encrypted bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		_ = gz.Close()
		_ = Encrypt(&encrypted, &compressed, key)
		file := filepath.Join(t.TempDir(), "bad.cbackup")
		_ = os.WriteFile(file, encrypted.Bytes(), 0o600)
		out := filepath.Join(t.TempDir(), "out")
		if err := Restore(context.Background(), file, keyFile, out); err == nil {
			t.Fatal("unsafe archive accepted")
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unsafe restore published files")
		}
	}
}

func TestStateScheduleKeysAndQueue(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(dir)
	if err != nil || c.Enabled || c.Hour != 3 || c.Weekday != 1 || c.Retain != 3 {
		t.Fatalf("defaults=%+v err=%v", c, err)
	}
	key, err := EnsureKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := EnsureKey(dir)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("re-download rotated recovery key")
	}
	instance, _ := NewInstance()
	c = Config{Enabled: true, Repository: "owner/private", Hour: 3, Weekday: 1, Retain: 3, KeySaved: true, Instance: instance}
	if err = SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	loaded, _ := LoadConfig(dir)
	if !reflect.DeepEqual(c, loaded) {
		t.Fatal("config not persisted")
	}
	if err = Request(dir); err != nil || !Pending(dir) {
		t.Fatal("request not persisted")
	}
	if err = Request(dir); err == nil {
		t.Fatal("duplicate pending request accepted")
	}
	for _, repository := range []string{"https://github.com/o/r", "o/r.git", "../evil", "o/r/x", "o/r\n"} {
		bad := c
		bad.Repository = repository
		if Validate(bad) == nil {
			t.Fatal("invalid repository accepted")
		}
	}
	bad := c
	bad.KeySaved = false
	if Validate(bad) == nil {
		t.Fatal("key consent bypassed")
	}
	zone, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 10, 5, 3, 0, 0, 0, zone)
	if !Due(c, Status{}, now) || Due(c, Status{}, now.Add(-time.Minute)) || Due(c, Status{Repository: c.Repository, LastAttemptDay: "2026-10-05"}, now) {
		t.Fatal("weekly schedule mismatch")
	}
	c.Enabled = false
	if Due(c, Status{}, now) {
		t.Fatal("disabled scheduled job ran")
	}
}

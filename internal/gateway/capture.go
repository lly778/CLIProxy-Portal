package gateway

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	CaptureLimit = 16 << 20
	captureChunk = 64 << 10
	captureMagic = "CPGW1"
)

type Vault struct {
	dir       string
	sharedDir string
	aead      cipher.AEAD
	hmacKey   [32]byte
	sharedMu  sync.Mutex
}

// Capture persistence and retention cleanup hold this lock across both file
// changes and their SQLite references, so a shared blob cannot be removed
// between reuse and indexing.
func (v *Vault) LockSharedMessages()   { v.sharedMu.Lock() }
func (v *Vault) UnlockSharedMessages() { v.sharedMu.Unlock() }

func NewVault(dir string, secret []byte) (*Vault, error) {
	if dir == "" || len(secret) < 32 {
		return nil, errors.New("gateway capture directory and app secret are required")
	}
	key := sha256.Sum256(append(append([]byte(nil), secret...), []byte("cliproxy-portal/gateway-capture/v1")...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sharedDir := filepath.Join(dir, "shared")
	if err := os.MkdirAll(sharedDir, 0o700); err != nil {
		return nil, err
	}
	// Raw wire captures are temporary parsing inputs only. Discard any left by a
	// previous crash before accepting new traffic.
	for _, kind := range []string{"rawrequest", "rawresponse"} {
		matches, _ := filepath.Glob(filepath.Join(dir, "*."+kind+".enc"))
		for _, path := range matches {
			_ = os.Remove(path)
		}
	}
	// A distinct HMAC key makes blob names opaque and prevents cross-user sharing.
	hmacKey := sha256.Sum256(append(append([]byte(nil), secret...), []byte("cliproxy-portal/gateway-message-id/v1")...))
	return &Vault{dir: dir, sharedDir: sharedDir, aead: aead, hmacKey: hmacKey}, nil
}

func validCaptureID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (v *Vault) filePath(id, kind string) (string, error) {
	if !validCaptureID(id) || (kind != "request" && kind != "response" && kind != "rawrequest" && kind != "rawresponse") {
		return "", errors.New("invalid capture name")
	}
	return filepath.Join(v.dir, id+"."+kind+".enc"), nil
}

type CaptureWriter struct {
	file      *os.File
	aead      cipher.AEAD
	prefix    [8]byte
	sequence  uint32
	buffer    []byte
	written   int
	truncated bool
	failed    error
}

func (v *Vault) Create(id, kind string) (*CaptureWriter, error) {
	path, err := v.filePath(id, kind)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	w, err := v.newWriter(file)
	if err != nil {
		_ = os.Remove(path)
	}
	return w, err
}

func (v *Vault) newWriter(file *os.File) (*CaptureWriter, error) {
	w := &CaptureWriter{file: file, aead: v.aead, buffer: make([]byte, 0, captureChunk)}
	var err error
	if _, err = rand.Read(w.prefix[:]); err == nil {
		_, err = file.Write(append([]byte(captureMagic), w.prefix[:]...))
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return w, nil
}

// Write never interrupts the proxied request when local capture storage fails.
func (w *CaptureWriter) Write(p []byte) (int, error) {
	inputSize := len(p)
	if w == nil || w.failed != nil || w.file == nil {
		return inputSize, nil
	}
	if len(p) > CaptureLimit-w.written {
		p = p[:CaptureLimit-w.written]
		w.truncated = true
	}
	w.written += len(p)
	for len(p) > 0 {
		n := min(len(p), captureChunk-len(w.buffer))
		w.buffer = append(w.buffer, p[:n]...)
		p = p[n:]
		if len(w.buffer) == captureChunk {
			if err := w.flush(w.buffer); err != nil {
				w.failed = err
				break
			}
			w.buffer = w.buffer[:0]
		}
	}
	return inputSize, nil
}

func (w *CaptureWriter) flush(plain []byte) error {
	var nonce [12]byte
	copy(nonce[:8], w.prefix[:])
	binary.BigEndian.PutUint32(nonce[8:], w.sequence)
	w.sequence++
	sealed := w.aead.Seal(nil, nonce[:], plain, nil)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
	if _, err := w.file.Write(length[:]); err != nil {
		return err
	}
	_, err := w.file.Write(sealed)
	return err
}

func (w *CaptureWriter) Close() (truncated bool, err error) {
	if w == nil || w.file == nil {
		return false, nil
	}
	if w.failed == nil && len(w.buffer) > 0 {
		w.failed = w.flush(w.buffer)
	}
	if w.failed == nil {
		// Authenticated empty final chunk detects an incomplete capture file.
		w.failed = w.flush(nil)
	}
	if closeErr := w.file.Close(); w.failed == nil {
		w.failed = closeErr
	}
	w.file = nil
	return w.truncated, w.failed
}

func (v *Vault) Read(id, kind string) ([]byte, error) {
	path, err := v.filePath(id, kind)
	if err != nil {
		return nil, err
	}
	return v.readPath(path)
}

func (v *Vault) readPath(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var header [13]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, err
	}
	if string(header[:5]) != captureMagic {
		return nil, errors.New("invalid capture header")
	}
	var out bytes.Buffer
	for sequence := uint32(0); ; sequence++ {
		var length [4]byte
		if _, err := io.ReadFull(file, length[:]); err != nil {
			return nil, err
		}
		size := binary.BigEndian.Uint32(length[:])
		if size < uint32(v.aead.Overhead()) || size > captureChunk+uint32(v.aead.Overhead()) {
			return nil, errors.New("invalid capture chunk size")
		}
		sealed := make([]byte, size)
		if _, err := io.ReadFull(file, sealed); err != nil {
			return nil, err
		}
		var nonce [12]byte
		copy(nonce[:8], header[5:])
		binary.BigEndian.PutUint32(nonce[8:], sequence)
		plain, err := v.aead.Open(nil, nonce[:], sealed, nil)
		if err != nil {
			return nil, fmt.Errorf("capture integrity check failed: %w", err)
		}
		if len(plain) == 0 {
			var tail [1]byte
			if n, tailErr := file.Read(tail[:]); n != 0 || tailErr != io.EOF {
				return nil, errors.New("trailing capture data")
			}
			return out.Bytes(), nil
		}
		if out.Len()+len(plain) > CaptureLimit {
			return nil, errors.New("capture exceeds limit")
		}
		_, _ = out.Write(plain)
	}
}

func (v *Vault) sharedMessageID(userID, role, text string) string {
	mac := hmac.New(sha256.New, v.hmacKey[:])
	for _, part := range []string{userID, role, text} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func (v *Vault) sharedMessagePath(id string) (string, error) {
	if len(id) != 64 {
		return "", errors.New("invalid shared message ID")
	}
	for _, ch := range id {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return "", errors.New("invalid shared message ID")
		}
	}
	return filepath.Join(v.sharedDir, id+".enc"), nil
}

// SaveSharedMessage stores one encrypted message once for one user. A request
// keeps ordered references in SQLite; no plaintext or cross-user blobs remain.
func (v *Vault) SaveSharedMessage(userID, role, text string) (string, bool, error) {
	if userID == "" || role == "" || text == "" || len(text) > CaptureLimit {
		return "", false, errors.New("invalid shared message")
	}
	id := v.sharedMessageID(userID, role, text)
	path, _ := v.sharedMessagePath(id)
	if _, err := os.Stat(path); err == nil {
		return id, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	file, err := os.CreateTemp(v.sharedDir, ".message-*")
	if err != nil {
		return "", false, err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	w, err := v.newWriter(file)
	if err != nil {
		return "", false, err
	}
	_, _ = w.Write([]byte(text))
	if truncated, err := w.Close(); err != nil {
		return "", false, fmt.Errorf("save shared message: %w", err)
	} else if truncated {
		return "", false, errors.New("shared message exceeded capture limit")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (v *Vault) ReadSharedMessage(userID, role, id string) (string, error) {
	path, err := v.sharedMessagePath(id)
	if err != nil {
		return "", err
	}
	plain, err := v.readPath(path)
	if err != nil {
		return "", err
	}
	if v.sharedMessageID(userID, role, string(plain)) != id {
		return "", errors.New("shared message owner or content mismatch")
	}
	return string(plain), nil
}

func (v *Vault) DeleteSharedMessage(id string) {
	if path, err := v.sharedMessagePath(id); err == nil {
		_ = os.Remove(path)
	}
}

func (v *Vault) Delete(id string) {
	for _, kind := range []string{"request", "response", "rawrequest", "rawresponse"} {
		if path, err := v.filePath(id, kind); err == nil {
			_ = os.Remove(path)
		}
	}
}

// PruneOrphans removes only aged, unindexed final capture files in this
// dedicated directory. The delay avoids racing an active capture being saved.
func (v *Vault) PruneOrphans(indexed map[string]bool, before time.Time) error {
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		for _, suffix := range []string{".request.enc", ".response.enc"} {
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			id := strings.TrimSuffix(name, suffix)
			if !validCaptureID(id) || indexed[id] {
				break
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(before) {
				if err := os.Remove(filepath.Join(v.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			break
		}
	}
	return nil
}

func (v *Vault) PruneSharedOrphans(referenced map[string]bool, before time.Time) error {
	entries, err := os.ReadDir(v.sharedDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		id := strings.TrimSuffix(name, ".enc")
		if !strings.HasSuffix(name, ".enc") && !strings.HasPrefix(name, ".message-") {
			continue
		}
		if strings.HasSuffix(name, ".enc") {
			if _, err := v.sharedMessagePath(id); err != nil || referenced[id] {
				continue
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(before) {
			if err := os.Remove(filepath.Join(v.sharedDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamRestoreRejectsLateAuthenticationAndGzipFailures(t *testing.T) {
	sources, dir := fixture(t)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	keyFile := filepath.Join(dir, "recovery.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, _, err := Create(t.Context(), sources, work, key)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	badTag := append([]byte(nil), valid...)
	badTag[len(badTag)-1] ^= 1
	var compressed, badGzip bytes.Buffer
	if err := Decrypt(&compressed, bytes.NewReader(valid), key); err != nil {
		t.Fatal(err)
	}
	compressed.Bytes()[compressed.Len()-8] ^= 1 // CRC corrupted, AES envelope remains valid.
	if err := Encrypt(&badGzip, &compressed, key); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"missing-authenticated-terminal": valid[:len(valid)-20],
		"extra-ciphertext":               append(append([]byte(nil), valid...), 0),
		"invalid-terminal-tag":           badTag,
		"gzip-checksum":                  badGzip.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, name+".cbackup")
			output := filepath.Join(dir, name+"-output")
			if err := os.WriteFile(file, body, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- Restore(t.Context(), file, keyFile, output) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("invalid stream accepted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("decrypt producer deadlocked on validation failure")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unauthenticated output promoted")
			}
			children, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				if strings.HasPrefix(child.Name(), ".portal-restore-") {
					t.Fatal("failed restore left plaintext staging")
				}
			}
		})
	}
}

func TestDecryptedStreamEarlyCloseAndCancellationJoinProducer(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	var ciphertext bytes.Buffer
	if err := Encrypt(&ciphertext, bytes.NewReader(bytes.Repeat([]byte{42}, chunkSize*3)), key); err != nil {
		t.Fatal(err)
	}
	for _, cancel := range []bool{false, true} {
		ctx, stop := context.WithCancel(t.Context())
		stream := newDecryptedStream(ctx, bytes.NewReader(ciphertext.Bytes()), key)
		if cancel {
			stop()
		}
		done := make(chan error, 1)
		go func() { done <- stream.Close() }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("early close did not join decrypt producer")
		}
		stop()
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	stream := newDecryptedStream(ctx, bytes.NewReader(ciphertext.Bytes()), key)
	if err := stream.finish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	_ = stream.Close()
}

func TestEncryptionBuffersDoNotAllocatePerFrame(t *testing.T) {
	key := bytes.Repeat([]byte{6}, 32)
	measure := func(frames int) (float64, float64) {
		plain := bytes.Repeat([]byte{4}, frames*chunkSize)
		var encrypted bytes.Buffer
		if err := Encrypt(&encrypted, bytes.NewReader(plain), key); err != nil {
			t.Fatal(err)
		}
		seal := testing.AllocsPerRun(3, func() {
			if err := Encrypt(io.Discard, bytes.NewReader(plain), key); err != nil {
				t.Fatal(err)
			}
		})
		open := testing.AllocsPerRun(3, func() {
			if err := Decrypt(io.Discard, bytes.NewReader(encrypted.Bytes()), key); err != nil {
				t.Fatal(err)
			}
		})
		return seal, open
	}
	smallSeal, smallOpen := measure(1)
	largeSeal, largeOpen := measure(64)
	if largeSeal > smallSeal+10 || largeOpen > smallOpen+10 {
		t.Fatalf("per-frame allocations: seal %.0f -> %.0f, open %.0f -> %.0f", smallSeal, largeSeal, smallOpen, largeOpen)
	}
}

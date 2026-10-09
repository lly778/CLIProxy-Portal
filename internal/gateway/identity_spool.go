package gateway

import (
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Large bodies use private, disk-backed files rather than a byte-size cutoff.
// On Unix unlink immediately: even a crash leaves no request plaintext files.
type identitySpools struct {
	dir   string
	files []*os.File
}

func (s *identitySpools) create() (*os.File, error) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.dir, "body-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil && runtime.GOOS != "windows" {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	s.files = append(s.files, f)
	return f, nil
}

func (s *identitySpools) keep(keep *os.File) {
	remaining := s.files[:0]
	for _, f := range s.files {
		if f == keep {
			remaining = append(remaining, f)
			continue
		}
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	s.files = remaining
}

func (s *identitySpools) close() { s.keep(nil) }

func (g *Gateway) identitySpools() *identitySpools {
	// The capture volume is disk-backed; container /tmp can be a small tmpfs.
	return &identitySpools{dir: filepath.Join(g.vault.dir, ".identity-buffer")}
}

type identityContextReader struct {
	ctx context.Context
	r   io.Reader
}

type identityContextReaderAt struct {
	ctx context.Context
	r   io.ReaderAt
}

func (r identityContextReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.ReadAt(p, offset)
}

func (r identityContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func identityCopy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(dst, identityContextReader{ctx, src}, make([]byte, 32<<10))
}

func decodeIdentitySpool(ctx context.Context, spools *identitySpools, wire *os.File, size int64, encoding string) (*os.File, int64, error) {
	current := wire
	encodings := strings.Split(encoding, ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		var reader io.ReadCloser
		var err error
		switch strings.ToLower(strings.TrimSpace(encodings[i])) {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			reader, err = gzip.NewReader(io.NewSectionReader(current, 0, size))
		case "deflate":
			reader, err = zlib.NewReader(io.NewSectionReader(current, 0, size))
			if err != nil {
				reader = flate.NewReader(io.NewSectionReader(current, 0, size))
				err = nil
			}
		default:
			return nil, 0, errors.New("unsupported identity content encoding")
		}
		if err != nil {
			return nil, 0, err
		}
		decoded, err := spools.create()
		if err != nil {
			_ = reader.Close()
			return nil, 0, err
		}
		n, err := identityCopy(ctx, decoded, reader)
		_ = reader.Close()
		if err != nil {
			return nil, 0, err
		}
		// Keep the original wire file for byte-exact fallback. Release previous
		// intermediate layers so nested encodings do not accumulate on disk.
		if current != wire {
			_ = current.Close()
			_ = os.Remove(current.Name())
		}
		current, size = decoded, n
	}
	return current, size, nil
}

type identitySpan struct{ start, end int64 }

func writeIdentitySpan(w io.Writer, span identitySpan) error {
	var raw [16]byte
	binary.LittleEndian.PutUint64(raw[:8], uint64(span.start))
	binary.LittleEndian.PutUint64(raw[8:], uint64(span.end))
	_, err := w.Write(raw[:])
	return err
}

func readIdentitySpan(r io.Reader) (identitySpan, error) {
	var raw [16]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return identitySpan{}, err
	}
	return identitySpan{int64(binary.LittleEndian.Uint64(raw[:8])), int64(binary.LittleEndian.Uint64(raw[8:]))}, nil
}

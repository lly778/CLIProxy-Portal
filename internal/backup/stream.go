package backup

import (
	"context"
	"io"
	"sync"
)

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// Closing the consumer always unblocks and joins the decrypting producer.
// Nothing is promoted from staging until finish authenticates EOF as well.
type decryptedStream struct {
	*io.PipeReader
	done <-chan error
	once sync.Once
	err  error
}

func newDecryptedStream(ctx context.Context, input io.Reader, key []byte) *decryptedStream {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Decrypt(writer, contextReader{ctx, input}, key)
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	return &decryptedStream{PipeReader: reader, done: done}
}

func (s *decryptedStream) wait() error {
	s.once.Do(func() { s.err = <-s.done })
	return s.err
}

func (s *decryptedStream) finish() error {
	if _, err := io.Copy(io.Discard, s.PipeReader); err != nil {
		return err
	}
	return s.wait()
}

func (s *decryptedStream) Close() error {
	_ = s.PipeReader.Close()
	return s.wait()
}

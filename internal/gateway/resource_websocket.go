package gateway

import (
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
)

var errResourceFrame = errors.New("websocket message exceeds resource policy or has invalid framing")

// ReverseProxy writes masked downstream frames into the upgraded upstream body.
// Validate headers before forwarding their payload. Compression is not negotiated
// under protection, so a small compressed frame cannot bypass the message limit.
type resourceWebsocket struct {
	io.ReadWriteCloser
	limit                       uint64
	header                      []byte
	remaining, message          uint64
	messageOpen, final, control bool
	tooLarge                    atomic.Bool
	closeSent                   bool
	onReject                    func()
}

func (w *resourceWebsocket) reject() (int, error) {
	if w.tooLarge.CompareAndSwap(false, true) {
		w.onReject()
	}
	_ = w.ReadWriteCloser.Close()
	return 0, errResourceFrame
}
func (w *resourceWebsocket) Read(p []byte) (int, error) {
	n, err := w.ReadWriteCloser.Read(p)
	if w.tooLarge.Load() && !w.closeSent && len(p) >= 4 {
		w.closeSent = true
		copy(p, []byte{0x88, 0x02, 0x03, 0xf1})
		return 4, nil
	}
	return n, err
}
func (w *resourceWebsocket) Write(p []byte) (int, error) {
	consumed := 0
	for len(p) > 0 {
		if w.remaining > 0 {
			n := len(p)
			if uint64(n) > w.remaining {
				n = int(w.remaining)
			}
			written, err := w.ReadWriteCloser.Write(p[:n])
			consumed += written
			w.remaining -= uint64(written)
			p = p[written:]
			if err != nil {
				return consumed, err
			}
			if written != n {
				return consumed, io.ErrShortWrite
			}
			if w.remaining == 0 {
				w.endFrame()
			}
			continue
		}
		w.header = append(w.header, p[0])
		p = p[1:]
		consumed++
		if len(w.header) < 2 {
			continue
		}
		lengthBytes := 0
		code := w.header[1] & 127
		if code == 126 {
			lengthBytes = 2
		} else if code == 127 {
			lengthBytes = 8
		}
		need := 2 + lengthBytes + 4
		if len(w.header) < need {
			continue
		}
		first := w.header[0]
		opcode := first & 15
		w.final = first&128 != 0
		w.control = opcode >= 8
		length := uint64(code)
		if lengthBytes == 2 {
			length = uint64(binary.BigEndian.Uint16(w.header[2:4]))
		} else if lengthBytes == 8 {
			length = binary.BigEndian.Uint64(w.header[2:10])
		}
		invalid := first&112 != 0 || w.header[1]&128 == 0 || length>>63 != 0
		if w.control {
			invalid = invalid || !w.final || length > 125 || (opcode != 8 && opcode != 9 && opcode != 10)
		} else {
			invalid = invalid || (opcode != 0 && opcode != 1 && opcode != 2) || (opcode == 0 && !w.messageOpen) || (opcode != 0 && w.messageOpen)
			if opcode != 0 {
				w.message = 0
				w.messageOpen = true
			}
			if length > w.limit-w.message {
				invalid = true
			} else {
				w.message += length
			}
		}
		if invalid {
			_, err := w.reject()
			return consumed, err
		}
		if n, err := w.ReadWriteCloser.Write(w.header); err != nil {
			return consumed, err
		} else if n != len(w.header) {
			return consumed, io.ErrShortWrite
		}
		w.header = w.header[:0]
		w.remaining = length
		if length == 0 {
			w.endFrame()
		}
	}
	return consumed, nil
}
func (w *resourceWebsocket) endFrame() {
	if !w.control && w.final {
		w.messageOpen = false
		w.message = 0
	}
}

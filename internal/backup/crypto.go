package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
)

const chunkSize = 64 << 10
const magic = "CPBACK01"

// Each frame authenticates its index and the random per-file nonce prefix.
// An authenticated empty terminal frame detects truncation and missing EOF.
func Encrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	header := make([]byte, len(magic)+8)
	copy(header, magic)
	if _, err = rand.Read(header[len(magic):]); err != nil {
		return err
	}
	if _, err = dst.Write(header); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	sealedBuffer := make([]byte, 0, chunkSize+aead.Overhead())
	var nonce [12]byte
	copy(nonce[:8], header[len(magic):])
	aad := make([]byte, len(header)+4)
	copy(aad, header)
	var length [4]byte
	for seq := uint32(0); ; seq++ {
		n, readErr := io.ReadFull(src, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if seq == ^uint32(0) {
			return errors.New("backup too large")
		}
		binary.BigEndian.PutUint32(nonce[8:], seq)
		copy(aad[len(header):], nonce[8:])
		sealed := aead.Seal(sealedBuffer[:0], nonce[:], buf[:n], aad)
		binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
		if _, err = dst.Write(length[:]); err != nil {
			return err
		}
		if _, err = dst.Write(sealed); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	header := make([]byte, len(magic)+8)
	if _, err = io.ReadFull(src, header); err != nil || string(header[:len(magic)]) != magic {
		return errors.New("备份文件格式无效")
	}
	sealedBuffer := make([]byte, chunkSize+aead.Overhead())
	plainBuffer := make([]byte, 0, chunkSize)
	var nonce [12]byte
	copy(nonce[:8], header[len(magic):])
	aad := make([]byte, len(header)+4)
	copy(aad, header)
	var length [4]byte
	for seq := uint32(0); ; seq++ {
		if _, err = io.ReadFull(src, length[:]); err != nil {
			return errors.New("备份文件不完整")
		}
		n := binary.BigEndian.Uint32(length[:])
		if n < uint32(aead.Overhead()) || n > chunkSize+uint32(aead.Overhead()) || seq == ^uint32(0) {
			return errors.New("备份数据帧无效")
		}
		sealed := sealedBuffer[:n]
		if _, err = io.ReadFull(src, sealed); err != nil {
			return errors.New("备份文件不完整")
		}
		binary.BigEndian.PutUint32(nonce[8:], seq)
		copy(aad[len(header):], nonce[8:])
		plain, err := aead.Open(plainBuffer[:0], nonce[:], sealed, aad)
		if err != nil {
			return errors.New("恢复密钥不正确或备份已损坏")
		}
		if len(plain) == 0 {
			var extra [1]byte
			if n, err := src.Read(extra[:]); n != 0 || err != io.EOF {
				return errors.New("备份文件存在额外数据")
			}
			return nil
		}
		if _, err = dst.Write(plain); err != nil {
			return err
		}
	}
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("恢复密钥必须为 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

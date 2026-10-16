// Package zipcrypt implements the two zip encryption schemes as streaming
// writers that sit between a compressor and the archive:
//
//   - WinZip AES (AE-1/AE-2, AES-256): salt, password verifier, AES-CTR
//     with a little-endian counter, HMAC-SHA1 authentication code.
//   - Traditional PKWARE encryption ("ZipCrypto").
//
// The writers produce the exact entry payload; the caller is responsible for
// the zip headers (method 99 plus the 0x9901 extra field for AES, the
// encryption flag bit for both).
package zipcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
)

// MethodAES is the zip compression method ID marking WinZip AES entries.
const MethodAES = 99

// AES-256 parameters of the WinZip AE format.
const (
	aesKeyLen   = 32
	aesSaltLen  = 16
	aesPVLen    = 2
	aesAuthLen  = 10
	aesStrength = 3 // 1 = 128, 2 = 192, 3 = 256 bit
	pbkdf2Iter  = 1000
)

// AESOverhead is the number of bytes AES adds to an entry's payload.
const AESOverhead = aesSaltLen + aesPVLen + aesAuthLen

// ZipCryptoOverhead is the size of the ZipCrypto encryption header.
const ZipCryptoOverhead = 12

// AESExtra returns the 0x9901 extra field for an AES entry. version is 1
// (AE-1, CRC stored) or 2 (AE-2, CRC zero); method is the real compression
// method of the encrypted data.
func AESExtra(version, method uint16) []byte {
	b := make([]byte, 11)
	binary.LittleEndian.PutUint16(b[0:], 0x9901)
	binary.LittleEndian.PutUint16(b[2:], 7)
	binary.LittleEndian.PutUint16(b[4:], version)
	b[6], b[7] = 'A', 'E'
	b[8] = aesStrength
	binary.LittleEndian.PutUint16(b[9:], method)
	return b
}

// aesWriter encrypts a stream in the WinZip AES format.
type aesWriter struct {
	w       io.Writer
	head    []byte // salt + password verifier, written before the data
	block   cipher.Block
	ctr     [2]uint64 // little-endian 128-bit counter
	ks      []byte    // unused keystream
	ksBuf   []byte
	mac     hash.Hash
	buf     []byte
	started bool
	closed  bool
}

const keystreamBlocks = 256 // 4 KiB of keystream per refill

// NewAESWriter returns a writer that encrypts everything written to it into
// w. Close writes the authentication code; it does not close w. Nothing is
// written to w before the first Write or Close, which allows using it as a
// compressor for archive/zip (that calls the compressor before writing the
// local header).
func NewAESWriter(w io.Writer, password string) (io.WriteCloser, error) {
	if password == "" {
		return nil, errors.New("zipcrypt: empty password")
	}
	salt := make([]byte, aesSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha1.New, password, salt, pbkdf2Iter, 2*aesKeyLen+aesPVLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:aesKeyLen])
	if err != nil {
		return nil, err
	}
	return &aesWriter{
		w:     w,
		head:  append(salt, key[2*aesKeyLen:]...),
		block: block,
		ctr:   [2]uint64{1, 0},
		ksBuf: make([]byte, keystreamBlocks*aes.BlockSize),
		mac:   hmac.New(sha1.New, key[aesKeyLen:2*aesKeyLen]),
	}, nil
}

func (a *aesWriter) start() error {
	if a.started {
		return nil
	}
	a.started = true
	_, err := a.w.Write(a.head)
	return err
}

func (a *aesWriter) refill() {
	var ctr [aes.BlockSize]byte
	for i := 0; i < keystreamBlocks; i++ {
		binary.LittleEndian.PutUint64(ctr[0:], a.ctr[0])
		binary.LittleEndian.PutUint64(ctr[8:], a.ctr[1])
		a.block.Encrypt(a.ksBuf[i*aes.BlockSize:], ctr[:])
		a.ctr[0]++
		if a.ctr[0] == 0 {
			a.ctr[1]++
		}
	}
	a.ks = a.ksBuf
}

func (a *aesWriter) Write(p []byte) (int, error) {
	if a.closed {
		return 0, errors.New("zipcrypt: write after close")
	}
	if err := a.start(); err != nil {
		return 0, err
	}
	if cap(a.buf) < len(p) {
		a.buf = make([]byte, len(p))
	}
	out := a.buf[:len(p)]
	for done := 0; done < len(p); {
		if len(a.ks) == 0 {
			a.refill()
		}
		n := subtle.XORBytes(out[done:], p[done:], a.ks)
		a.ks = a.ks[n:]
		done += n
	}
	a.mac.Write(out)
	if _, err := a.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *aesWriter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	if err := a.start(); err != nil {
		return err
	}
	_, err := a.w.Write(a.mac.Sum(nil)[:aesAuthLen])
	return err
}

// zipCryptoWriter encrypts a stream with traditional PKWARE encryption.
type zipCryptoWriter struct {
	w       io.Writer
	keys    [3]uint32
	head    [ZipCryptoOverhead]byte
	buf     []byte
	started bool
}

// NewZipCryptoWriter returns a ZipCrypto encrypting writer. check is the
// last byte of the encryption header that readers use to verify the
// password: the high byte of the CRC-32 when the CRC is known up front, or
// the high byte of the MS-DOS modification time when the entry uses a data
// descriptor (flag bit 3). Nothing is written before the first Write/Close.
func NewZipCryptoWriter(w io.Writer, password string, check byte) (io.WriteCloser, error) {
	if password == "" {
		return nil, errors.New("zipcrypt: empty password")
	}
	z := &zipCryptoWriter{w: w, keys: [3]uint32{0x12345678, 0x23456789, 0x34567890}}
	for i := 0; i < len(password); i++ {
		z.update(password[i])
	}
	if _, err := rand.Read(z.head[:ZipCryptoOverhead-1]); err != nil {
		return nil, err
	}
	z.head[ZipCryptoOverhead-1] = check
	return z, nil
}

func crc32Update(crc uint32, b byte) uint32 {
	return crc32.IEEETable[byte(crc)^b] ^ (crc >> 8)
}

func (z *zipCryptoWriter) update(b byte) {
	z.keys[0] = crc32Update(z.keys[0], b)
	z.keys[1] = (z.keys[1]+z.keys[0]&0xff)*134775813 + 1
	z.keys[2] = crc32Update(z.keys[2], byte(z.keys[1]>>24))
}

func (z *zipCryptoWriter) encrypt(dst, src []byte) {
	for i, b := range src {
		t := z.keys[2] | 2
		dst[i] = b ^ byte((t*(t^1))>>8)
		z.update(b)
	}
}

func (z *zipCryptoWriter) start() error {
	if z.started {
		return nil
	}
	z.started = true
	var h [ZipCryptoOverhead]byte
	z.encrypt(h[:], z.head[:])
	_, err := z.w.Write(h[:])
	return err
}

func (z *zipCryptoWriter) Write(p []byte) (int, error) {
	if err := z.start(); err != nil {
		return 0, err
	}
	if cap(z.buf) < len(p) {
		z.buf = make([]byte, len(p))
	}
	out := z.buf[:len(p)]
	z.encrypt(out, p)
	if _, err := z.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (z *zipCryptoWriter) Close() error { return z.start() }

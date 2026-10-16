package zipcrypt

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"hash/crc32"
	"io"
	"testing"

	yzip "github.com/yeka/zip"
)

const pw = "p@ss 中文"

func payloads() map[string][]byte {
	big := make([]byte, 3<<20+17)
	rand.Read(big)
	return map[string][]byte{"empty": {}, "tiny": []byte("hi"), "text": bytes.Repeat([]byte("hello zip "), 5000), "big": big}
}

// readBack decrypts every entry with the independent yeka/zip reader.
func readBack(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	r, err := yzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range r.File {
		if !f.IsEncrypted() {
			t.Fatalf("%s not encrypted", f.Name)
		}
		f.SetPassword(pw)
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = b
	}
	return out
}

func check(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	for k, v := range want {
		if !bytes.Equal(got[k], v) {
			t.Errorf("%s: content mismatch (%d vs %d bytes)", k, len(got[k]), len(v))
		}
	}
}

// Streaming mode: archive/zip computes the CRC (AE-1) and writes a data
// descriptor; ZipCrypto then verifies against the DOS time.
func TestStreamingWithStdWriter(t *testing.T) {
	for _, enc := range []string{"aes", "zipcrypto"} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		var cur *zip.FileHeader
		inner := func(w io.Writer) (io.WriteCloser, error) {
			if enc == "aes" {
				return NewAESWriter(w, pw)
			}
			return NewZipCryptoWriter(w, pw, byte(cur.ModifiedTime>>8))
		}
		zw.RegisterCompressor(MethodAES, inner)
		zw.RegisterCompressor(zip.Store, inner)
		want := payloads()
		for name, data := range want {
			fh := &zip.FileHeader{Name: name, Method: zip.Store, Flags: 0x1}
			fh.ModifiedTime, fh.ModifiedDate = 0xA5C3, 0x5B4F
			if enc == "aes" {
				fh.Method = MethodAES
				fh.Extra = AESExtra(1, zip.Store)
			}
			cur = fh
			w, err := zw.CreateHeader(fh)
			if err != nil {
				t.Fatal(err)
			}
			w.Write(data)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		check(t, readBack(t, buf.Bytes()), want)
	}
}

// Raw mode: payload is prepared up front; AE-2 (no CRC) and ZipCrypto
// verified against the CRC.
func TestRawPayloads(t *testing.T) {
	for _, enc := range []string{"aes", "zipcrypto"} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		want := payloads()
		for name, data := range want {
			crc := crc32.ChecksumIEEE(data)
			var payload bytes.Buffer
			var w io.WriteCloser
			fh := &zip.FileHeader{Name: name, Method: zip.Store, Flags: 0x1, CRC32: crc}
			if enc == "aes" {
				w, _ = NewAESWriter(&payload, pw)
				fh.Method, fh.CRC32 = MethodAES, 0
				fh.Extra = AESExtra(2, zip.Store)
			} else {
				w, _ = NewZipCryptoWriter(&payload, pw, byte(crc>>24))
			}
			w.Write(data)
			w.Close()
			fh.CompressedSize64 = uint64(payload.Len())
			fh.UncompressedSize64 = uint64(len(data))
			rw, err := zw.CreateRaw(fh)
			if err != nil {
				t.Fatal(err)
			}
			rw.Write(payload.Bytes())
		}
		zw.Close()
		check(t, readBack(t, buf.Bytes()), want)
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var payload bytes.Buffer
	w, _ := NewAESWriter(&payload, pw)
	w.Write([]byte("secret"))
	w.Close()
	fh := &zip.FileHeader{Name: "f", Method: MethodAES, Flags: 0x1, Extra: AESExtra(2, zip.Store),
		CompressedSize64: uint64(payload.Len()), UncompressedSize64: 6}
	rw, _ := zw.CreateRaw(fh)
	rw.Write(payload.Bytes())
	zw.Close()
	r, _ := yzip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r.File[0].SetPassword("wrong")
	if rc, err := r.File[0].Open(); err == nil {
		if _, err := io.ReadAll(rc); err == nil {
			t.Fatal("wrong password accepted")
		}
	}
}

func BenchmarkAES(b *testing.B) {
	data := make([]byte, 1<<20)
	b.SetBytes(int64(len(data)))
	w, _ := NewAESWriter(io.Discard, pw)
	for i := 0; i < b.N; i++ {
		w.Write(data)
	}
}

func BenchmarkZipCrypto(b *testing.B) {
	data := make([]byte, 1<<20)
	b.SetBytes(int64(len(data)))
	w, _ := NewZipCryptoWriter(io.Discard, pw, 0)
	for i := 0; i < b.N; i++ {
		w.Write(data)
	}
}

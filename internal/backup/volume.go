package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/yeka/zip"

	"my_sync/internal/config"
	"my_sync/internal/index"
)

// volumeWriter writes files into a sequence of standalone zip archives.
// Volumes are not a spanned multi-part archive: every volume is a complete
// zip file that can be extracted on its own. A file is never split across
// volumes, so a file larger than the volume size gets a volume of its own.
type volumeWriter struct {
	dir        string
	baseName   string // "<prefix>_<backup id>"
	limit      int64  // 0 = unlimited
	password   string
	encryption string
	method     uint16

	seq     int
	cur     *openVolume
	volumes []index.Volume
}

type openVolume struct {
	name  string
	f     *os.File
	cw    *countHashWriter
	zw    *zip.Writer
	files int
}

type countHashWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (c *countHashWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

func newVolumeWriter(dir, baseName string, limit int64, password, encryption, compression string) *volumeWriter {
	method := zip.Deflate
	if compression == config.CompressionStore {
		method = zip.Store
	}
	return &volumeWriter{
		dir:        dir,
		baseName:   baseName,
		limit:      limit,
		password:   password,
		encryption: encryption,
		method:     method,
	}
}

// entryOverhead estimates zip headers, encryption and descriptor bytes.
func entryOverhead(name string) int64 {
	return int64(2*len(name)) + 30 + 46 + 24 + 9 + 28 + 64
}

// estimateArchived is a conservative upper bound of the bytes a file adds to
// the archives: its data (deflate may expand incompressible input by a few
// bytes per 64 KiB block), local/central headers, encryption overhead and
// the end records of a volume it may open.
func estimateArchived(sf scannedFile) int64 {
	return sf.Size + sf.Size/4096 + entryOverhead(sf.Key) + 128
}

// written returns the bytes written to all volumes so far.
func (vw *volumeWriter) written() int64 {
	var n int64
	for _, v := range vw.volumes {
		n += v.Size
	}
	if vw.cur != nil {
		n += vw.cur.cw.n
	}
	return n
}

// add archives one file and returns the volume name and SHA-256 of the
// data written. The source file is opened before the zip entry is created
// so that unreadable files leave no trace in the archive.
func (vw *volumeWriter) add(ctx context.Context, sf scannedFile, onBytes func(int64)) (volume string, size int64, sum string, err error) {
	f, err := os.Open(sf.Abs)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()

	est := sf.Size + entryOverhead(sf.Key)
	if vw.cur != nil && vw.limit > 0 && vw.cur.files > 0 && vw.cur.cw.n+est > vw.limit {
		if err := vw.closeCurrent(); err != nil {
			return "", 0, "", writeError{err}
		}
	}
	if vw.cur == nil {
		if err := vw.openNext(); err != nil {
			return "", 0, "", writeError{err}
		}
	}

	fh := &zip.FileHeader{Name: sf.Key, Method: vw.method}
	setModTime(fh, time.Unix(0, sf.ModTime))
	fh.SetMode(sf.Mode.Perm())
	if !isASCII(sf.Key) && utf8.ValidString(sf.Key) {
		fh.Flags |= 0x800 // names are UTF-8
	}
	if vw.password != "" {
		fh.SetPassword(vw.password)
		if vw.encryption == config.EncryptionZipCrypto {
			fh.SetEncryptionMethod(zip.StandardEncryption)
		} else {
			fh.SetEncryptionMethod(zip.AES256Encryption)
		}
	}
	w, err := vw.cur.zw.CreateHeader(fh)
	if err != nil {
		return "", 0, "", writeError{err}
	}
	vw.cur.files++
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(errWrapWriter{w}, h), &ctxReader{ctx: ctx, r: f, onBytes: onBytes})
	if err != nil {
		return "", n, "", err
	}
	if err := vw.cur.zw.Flush(); err != nil {
		return "", n, "", writeError{err}
	}
	volume = vw.cur.name
	if vw.limit > 0 && vw.cur.cw.n >= vw.limit {
		if err := vw.closeCurrent(); err != nil {
			return "", n, "", writeError{err}
		}
	}
	return volume, n, hex.EncodeToString(h.Sum(nil)), nil
}

func (vw *volumeWriter) openNext() error {
	vw.seq++
	name := fmt.Sprintf("%s_%03d.zip", vw.baseName, vw.seq)
	f, err := os.OpenFile(filepath.Join(vw.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cw := &countHashWriter{w: f, h: sha256.New()}
	vw.cur = &openVolume{name: name, f: f, cw: cw, zw: zip.NewWriter(cw)}
	return nil
}

func (vw *volumeWriter) closeCurrent() error {
	v := vw.cur
	if v == nil {
		return nil
	}
	vw.cur = nil
	err := v.zw.Close()
	if err == nil {
		err = v.f.Sync()
	}
	if cerr := v.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("关闭压缩包 %s: %w", v.name, err)
	}
	vw.volumes = append(vw.volumes, index.Volume{
		Name:   v.name,
		Size:   v.cw.n,
		Files:  v.files,
		SHA256: hex.EncodeToString(v.cw.h.Sum(nil)),
	})
	return nil
}

// close finalizes the open volume and returns all volumes written.
func (vw *volumeWriter) close() ([]index.Volume, error) {
	err := vw.closeCurrent()
	return vw.volumes, err
}

// abort releases file handles without finalizing; the caller removes files.
func (vw *volumeWriter) abort() {
	if vw.cur != nil {
		vw.cur.f.Close()
		vw.cur = nil
	}
}

// setModTime stores the local MS-DOS time plus an "extended timestamp"
// extra field (0x5455) with the exact UTC time, which 7-Zip, unzip and
// most other tools prefer over the DOS fields.
func setModTime(fh *zip.FileHeader, t time.Time) {
	lt := t.Local()
	if lt.Year() < 1980 {
		lt = time.Date(1980, 1, 1, 0, 0, 0, 0, time.Local)
	}
	fh.ModifiedDate = uint16(lt.Day() + int(lt.Month())<<5 + (lt.Year()-1980)<<9)
	fh.ModifiedTime = uint16(lt.Second()/2 + lt.Minute()<<5 + lt.Hour()<<11)
	if u := t.Unix(); u >= 0 && u <= 0xFFFFFFFF {
		var extra [9]byte
		binary.LittleEndian.PutUint16(extra[0:], 0x5455)
		binary.LittleEndian.PutUint16(extra[2:], 5)
		extra[4] = 1 // modification time present
		binary.LittleEndian.PutUint32(extra[5:], uint32(u))
		fh.Extra = append(fh.Extra, extra[:]...)
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// errWrapWriter tags errors of the archive side as writeError.
type errWrapWriter struct{ w io.Writer }

func (e errWrapWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		err = writeError{err}
	}
	return n, err
}

// ctxReader aborts reads once the context is cancelled and reports progress.
type ctxReader struct {
	ctx     context.Context
	r       io.Reader
	onBytes func(int64)
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 && c.onBytes != nil {
		c.onBytes(int64(n))
	}
	return n, err
}

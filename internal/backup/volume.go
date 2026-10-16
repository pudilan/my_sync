package backup

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/flate"

	"my_sync/internal/config"
	"my_sync/internal/zipcrypt"
)

const (
	// smallFileLimit is the largest file prepared in memory by the parallel
	// workers; larger files are streamed by the writer.
	smallFileLimit = 8 << 20
	readBufSize    = 1 << 20
	writeBufSize   = 4 << 20
	zipVersion20   = 20
)

// entryOptions controls how file contents are encoded.
type entryOptions struct {
	password    string
	encryption  string
	compression string
	level       int
}

func newEntryOptions(cfg config.Config) entryOptions {
	return entryOptions{
		password:    cfg.Password,
		encryption:  cfg.Encryption,
		compression: cfg.Compression,
		level:       cfg.CompressionLevel,
	}
}

func (o entryOptions) aes() bool {
	return o.password != "" && o.encryption != config.EncryptionZipCrypto
}

// methodFor picks the compression method of a file.
func (o entryOptions) methodFor(name string) uint16 {
	switch o.compression {
	case config.CompressionStore:
		return zip.Store
	case config.CompressionAuto:
		if isCompressedExt(name) {
			return zip.Store
		}
	}
	return zip.Deflate
}

// markEncrypted sets the encryption flag; for AES the real method moves into
// the 0x9901 extra field and the header method becomes 99.
func (o entryOptions) markEncrypted(fh *zip.FileHeader, aeVersion uint16) {
	if o.password == "" {
		return
	}
	fh.Flags |= 0x1
	if o.aes() {
		fh.Extra = append(fh.Extra, zipcrypt.AESExtra(aeVersion, fh.Method)...)
		fh.Method = zipcrypt.MethodAES
	}
}

var flatePools [10]sync.Pool

func getFlate(w io.Writer, level int) *flate.Writer {
	if fw, ok := flatePools[level].Get().(*flate.Writer); ok {
		fw.Reset(w)
		return fw
	}
	fw, _ := flate.NewWriter(w, level)
	return fw
}

func putFlate(fw *flate.Writer, level int) { flatePools[level].Put(fw) }

// newHeader builds the common header fields of an entry.
func newHeader(sf scannedFile, method uint16) *zip.FileHeader {
	fh := &zip.FileHeader{Name: sf.Key, Method: method}
	setModTime(fh, time.Unix(0, sf.ModTime))
	fh.SetMode(sf.Mode.Perm())
	if !isASCII(sf.Key) && utf8.ValidString(sf.Key) {
		fh.Flags |= 0x800 // names are UTF-8
	}
	return fh
}

// preparedEntry is a fully compressed and encrypted entry ready to be
// copied into a volume.
type preparedEntry struct {
	fh      *zip.FileHeader
	payload []byte
	size    int64 // uncompressed bytes
	sha256  string
}

// prepareEntry reads a small file and encodes it in memory. It is safe for
// concurrent use. tooBig reports that the file grew beyond what is sensible
// to buffer; the caller then streams it instead.
func prepareEntry(ctx context.Context, sf scannedFile, o entryOptions, onBytes func(int64)) (e *preparedEntry, tooBig bool, err error) {
	f, err := os.Open(sf.Abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	const maxBuffered = 4 * smallFileLimit
	data := make([]byte, 0, sf.Size+1)
	buf := bytes.NewBuffer(data)
	if _, err := buf.ReadFrom(io.LimitReader(&ctxReader{ctx: ctx, r: f, onBytes: onBytes}, maxBuffered+1)); err != nil {
		return nil, false, err
	}
	data = buf.Bytes()
	if len(data) > maxBuffered {
		return nil, true, nil
	}
	sum := sha256.Sum256(data)
	crc := crc32.ChecksumIEEE(data)

	method := o.methodFor(sf.Key)
	body := data
	if method == zip.Deflate {
		var out bytes.Buffer
		out.Grow(len(data)/2 + 64)
		fw := getFlate(&out, o.level)
		fw.Write(data)
		fw.Close()
		putFlate(fw, o.level)
		if out.Len() < len(data) {
			body = out.Bytes()
		} else {
			method = zip.Store // incompressible: store as is
		}
	}
	fh := newHeader(sf, method)
	fh.CRC32 = crc
	if o.password != "" {
		var out bytes.Buffer
		out.Grow(len(body) + zipcrypt.AESOverhead)
		var w io.WriteCloser
		if o.aes() {
			w, err = zipcrypt.NewAESWriter(&out, o.password)
			fh.CRC32 = 0 // AE-2 does not store the CRC
		} else {
			w, err = zipcrypt.NewZipCryptoWriter(&out, o.password, byte(crc>>24))
		}
		if err != nil {
			return nil, false, err
		}
		w.Write(body)
		w.Close()
		body = out.Bytes()
		o.markEncrypted(fh, 2)
	}
	fh.CompressedSize64 = uint64(len(body))
	fh.UncompressedSize64 = uint64(len(data))
	fh.CreatorVersion = fh.CreatorVersion&0xff00 | zipVersion20
	fh.ReaderVersion = zipVersion20
	return &preparedEntry{fh: fh, payload: body, size: int64(len(data)), sha256: hex.EncodeToString(sum[:])}, false, nil
}

// volumeWriter writes files into a sequence of standalone zip archives.
// Volumes are not a spanned multi-part archive: every volume is a complete
// zip file that can be extracted on its own. A file is never split across
// volumes, so a file larger than the volume size gets a volume of its own.
// It is used by a single goroutine.
type volumeWriter struct {
	dir      string
	baseName string // "<prefix>_<backup id>"
	limit    int64  // 0 = unlimited
	opts     entryOptions

	seq     int
	cur     *openVolume
	volumes []volumeInfo

	// Header and real method of the entry being streamed, read by the
	// compressor that archive/zip invokes from CreateHeader.
	streamFH     *zip.FileHeader
	streamMethod uint16
	streamComp   io.WriteCloser
	readBuf      []byte
}

type volumeInfo struct {
	name   string
	size   int64
	files  int
	sha256 string
}

type openVolume struct {
	name  string
	f     *os.File
	bw    *bufio.Writer
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

func newVolumeWriter(dir, baseName string, limit int64, opts entryOptions) *volumeWriter {
	return &volumeWriter{dir: dir, baseName: baseName, limit: limit, opts: opts}
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

// preparedSize is the exact payload plus a bound for its headers.
func preparedSize(e *preparedEntry) int64 {
	return int64(len(e.payload)) + entryOverhead(e.fh.Name) + int64(2*len(e.fh.Extra)) + 128
}

// written returns the bytes written to all volumes so far.
func (vw *volumeWriter) written() int64 {
	var n int64
	for _, v := range vw.volumes {
		n += v.size
	}
	if vw.cur != nil {
		n += vw.cur.cw.n
	}
	return n
}

// ensureRoom starts a new volume if an entry of the given size would push
// the current one over the limit, and opens a volume if none is open.
func (vw *volumeWriter) ensureRoom(size int64) error {
	if vw.cur != nil && vw.limit > 0 && vw.cur.files > 0 && vw.cur.cw.n+size > vw.limit {
		if err := vw.closeCurrent(); err != nil {
			return writeError{err}
		}
	}
	if vw.cur == nil {
		if err := vw.openNext(); err != nil {
			return writeError{err}
		}
	}
	return nil
}

// afterEntry closes the volume once it reached the limit.
func (vw *volumeWriter) afterEntry() error {
	vw.cur.files++
	if vw.limit > 0 && vw.cur.cw.n >= vw.limit {
		if err := vw.closeCurrent(); err != nil {
			return writeError{err}
		}
	}
	return nil
}

// addPrepared copies an entry encoded by prepareEntry into the archive.
func (vw *volumeWriter) addPrepared(e *preparedEntry) (volume string, err error) {
	if err := vw.ensureRoom(preparedSize(e)); err != nil {
		return "", err
	}
	w, err := vw.cur.zw.CreateRaw(e.fh)
	if err != nil {
		return "", writeError{err}
	}
	if _, err := w.Write(e.payload); err != nil {
		return "", writeError{err}
	}
	volume = vw.cur.name
	return volume, vw.afterEntry()
}

// addStream archives a large file while reading it. The source file is
// opened before the zip entry is created so that unreadable files leave no
// trace in the archive.
func (vw *volumeWriter) addStream(ctx context.Context, sf scannedFile, onBytes func(int64)) (volume string, size int64, sum string, err error) {
	f, err := os.Open(sf.Abs)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	if err := vw.ensureRoom(estimateArchived(sf)); err != nil {
		return "", 0, "", err
	}

	method := vw.opts.methodFor(sf.Key)
	fh := newHeader(sf, method)
	vw.opts.markEncrypted(fh, 1) // AE-1: archive/zip stores the CRC
	vw.streamFH, vw.streamMethod = fh, method
	w, err := vw.cur.zw.CreateHeader(fh)
	if err != nil {
		return "", 0, "", writeError{err}
	}
	if vw.readBuf == nil {
		vw.readBuf = make([]byte, readBufSize)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(errWrapWriter{w}, h), &ctxReader{ctx: ctx, r: f, onBytes: onBytes}, vw.readBuf)
	if err != nil {
		return "", n, "", err
	}
	// Finish the entry now so that its full size counts for the volume and
	// per-run limits; archive/zip closes the compressor again later, which
	// is a no-op.
	if err := vw.streamComp.Close(); err != nil {
		return "", n, "", writeError{err}
	}
	if err := vw.cur.zw.Flush(); err != nil {
		return "", n, "", writeError{err}
	}
	volume = vw.cur.name
	if err := vw.afterEntry(); err != nil {
		return "", n, "", err
	}
	return volume, n, hex.EncodeToString(h.Sum(nil)), nil
}

// compressor encodes streamed entries: deflate (klauspost) and/or the
// configured encryption, chained as data -> deflate -> encrypt -> archive.
func (vw *volumeWriter) compressor(out io.Writer) (io.WriteCloser, error) {
	var enc io.WriteCloser = nopWriteCloser{out}
	if o := vw.opts; o.password != "" {
		var err error
		if o.aes() {
			enc, err = zipcrypt.NewAESWriter(out, o.password)
		} else {
			// Entries carry a data descriptor, so readers verify the
			// password against the DOS time instead of the CRC.
			enc, err = zipcrypt.NewZipCryptoWriter(out, o.password, byte(vw.streamFH.ModifiedTime>>8))
		}
		if err != nil {
			return nil, err
		}
	}
	if vw.streamMethod == zip.Deflate {
		enc = &chainWriter{fw: getFlate(enc, vw.opts.level), level: vw.opts.level, next: enc}
	}
	vw.streamComp = enc
	return enc, nil
}

// chainWriter deflates into an encrypting (or plain) writer. Close is
// idempotent.
type chainWriter struct {
	fw     *flate.Writer
	level  int
	next   io.WriteCloser
	closed bool
}

func (c *chainWriter) Write(p []byte) (int, error) { return c.fw.Write(p) }

func (c *chainWriter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.fw.Close()
	putFlate(c.fw, c.level)
	if cerr := c.next.Close(); err == nil {
		err = cerr
	}
	return err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (vw *volumeWriter) openNext() error {
	vw.seq++
	name := fmt.Sprintf("%s_%03d.zip", vw.baseName, vw.seq)
	f, err := os.OpenFile(filepath.Join(vw.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, writeBufSize)
	cw := &countHashWriter{w: bw, h: sha256.New()}
	zw := zip.NewWriter(cw)
	for _, m := range []uint16{zip.Store, zip.Deflate, zipcrypt.MethodAES} {
		zw.RegisterCompressor(m, vw.compressor)
	}
	vw.cur = &openVolume{name: name, f: f, bw: bw, cw: cw, zw: zw}
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
		err = v.bw.Flush()
	}
	if err == nil {
		err = v.f.Sync()
	}
	if cerr := v.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("关闭压缩包 %s: %w", v.name, err)
	}
	vw.volumes = append(vw.volumes, volumeInfo{
		name:   v.name,
		size:   v.cw.n,
		files:  v.files,
		sha256: hex.EncodeToString(v.cw.h.Sum(nil)),
	})
	return nil
}

// close finalizes the open volume.
func (vw *volumeWriter) close() error { return vw.closeCurrent() }

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

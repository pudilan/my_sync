package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yeka/zip"

	"my_sync/internal/index"
)

// RestoreOptions controls a restore.
type RestoreOptions struct {
	Dest     string // backup destination (where backup directories live)
	BackupID string // state to restore; empty = latest
	Target   string // directory to restore into
	Prefix   string // only restore paths starting with this prefix
	Password string
}

// RestoreResult summarises a restore.
type RestoreResult struct {
	BackupID string
	Files    int
	Bytes    int64
}

// Restore rebuilds the file tree as recorded in a backup index. Thanks to the
// index every file is read directly from the volume holding its latest
// version, so only the needed archives are opened.
func Restore(ctx context.Context, opts RestoreOptions, logf func(string)) (*RestoreResult, error) {
	var ix *index.Index
	var err error
	if opts.BackupID == "" {
		ix, err = index.Latest(opts.Dest)
		if err == nil && ix == nil {
			err = fmt.Errorf("%s 下没有备份", opts.Dest)
		}
	} else {
		ix, err = index.Load(filepath.Join(opts.Dest, opts.BackupID))
	}
	if err != nil {
		return nil, err
	}
	target, err := filepath.Abs(opts.Target)
	if err != nil {
		return nil, err
	}

	// Group wanted files by the volume that holds them.
	type want struct {
		path string
		e    index.FileEntry
	}
	byVolume := map[string][]want{}
	for p, e := range ix.Files {
		if opts.Prefix != "" && !strings.HasPrefix(p, opts.Prefix) {
			continue
		}
		vol := filepath.Join(opts.Dest, e.Backup, e.Volume)
		byVolume[vol] = append(byVolume[vol], want{p, e})
	}
	vols := make([]string, 0, len(byVolume))
	for v := range byVolume {
		vols = append(vols, v)
	}
	sort.Strings(vols)

	res := &RestoreResult{BackupID: ix.BackupID}
	for _, vol := range vols {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		wants := map[string]index.FileEntry{}
		for _, w := range byVolume[vol] {
			wants[w.path] = w.e
		}
		logf(fmt.Sprintf("从 %s 恢复 %d 个文件", vol, len(wants)))
		if err := restoreVolume(ctx, vol, wants, target, opts.Password, res); err != nil {
			return res, err
		}
		if len(wants) > 0 {
			return res, fmt.Errorf("%s 中缺少 %d 个文件", vol, len(wants))
		}
	}
	return res, nil
}

func restoreVolume(ctx context.Context, vol string, wants map[string]index.FileEntry, target, password string, res *RestoreResult) error {
	r, err := zip.OpenReader(vol)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		e, ok := wants[f.Name]
		if !ok {
			continue
		}
		out := filepath.Join(target, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(out, target+string(filepath.Separator)) {
			return fmt.Errorf("非法路径 %q", f.Name)
		}
		if f.IsEncrypted() {
			if password == "" {
				return fmt.Errorf("%s 已加密，需要提供密码", vol)
			}
			f.SetPassword(password)
		}
		if err := extractFile(ctx, f, out, e); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		delete(wants, f.Name)
		res.Files++
		res.Bytes += e.Size
	}
	return nil
}

func extractFile(ctx context.Context, f *zip.File, out string, e index.FileEntry) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(w, h), &ctxReader{ctx: ctx, r: rc})
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); e.SHA256 != "" && sum != e.SHA256 {
		return fmt.Errorf("校验失败 (sha256 %s != %s)", sum, e.SHA256)
	}
	mt := time.Unix(0, e.ModTime)
	return os.Chtimes(out, mt, mt)
}

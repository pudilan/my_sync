// Package backup implements incremental, encrypted, volume-split backups.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"my_sync/internal/config"
	"my_sync/internal/index"
)

// Phases reported through Progress.
const (
	PhaseScanning   = "scanning"
	PhaseComparing  = "comparing"
	PhaseArchiving  = "archiving"
	PhaseFinalizing = "finalizing"
)

const tmpPrefix = ".tmp-"

// Progress is a snapshot of a running backup.
type Progress struct {
	Phase      string `json:"phase"`
	FilesTotal int    `json:"files_total"`
	FilesDone  int    `json:"files_done"`
	BytesTotal int64  `json:"bytes_total"`
	BytesDone  int64  `json:"bytes_done"`
	Current    string `json:"current"`
}

// Reporter receives log lines and progress updates. Methods may be called
// frequently and must not block.
type Reporter interface {
	Log(msg string)
	Progress(p Progress)
}

// Options alters a single run.
type Options struct {
	// DryRun only computes what would be backed up.
	DryRun bool
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Result describes the outcome of a run.
type Result struct {
	BackupID  string `json:"backup_id,omitempty"`
	Dir       string `json:"dir,omitempty"`
	NoChanges bool   `json:"no_changes"`
	DryRun    bool   `json:"dry_run"`
	Added     int    `json:"added"`
	Modified  int    `json:"modified"`
	Deleted   int    `json:"deleted"`
	Bytes     int64  `json:"bytes"`
	// Deferred changes are left for the next run (max_backup_size);
	// Oversize files exceed the limit on their own and are never archived.
	Deferred      int            `json:"deferred"`
	DeferredBytes int64          `json:"deferred_bytes"`
	Oversize      int            `json:"oversize"`
	Summary       *index.Summary `json:"summary,omitempty"`
	// Changes is filled for dry runs (capped) so the UI can preview them.
	Changes []index.Change `json:"changes,omitempty"`
}

// MaxPreviewChanges caps the change list returned by a dry run.
const MaxPreviewChanges = 2000

type plan struct {
	added, modified []scannedFile
	deleted         []string
	touched         map[string]int64 // same content, new mtime
	scanned         []scannedFile
	bytes           int64

	// Filled by selectWithinLimit.
	todo          []scannedFile // files archived by this run, sorted by key
	todoBytes     int64
	ops           map[string]string
	deferred      []scannedFile
	deferredBytes int64
	oversize      []scannedFile
}

func (p *plan) empty() bool {
	return len(p.added) == 0 && len(p.modified) == 0 && len(p.deleted) == 0
}

// selectWithinLimit picks the files archived by this run so that the
// estimated archive size stays within limit (0 = unlimited). Files are taken
// in path order; a file that does not fit is deferred while smaller files
// after it may still be taken. Files that cannot fit even into an empty run
// are reported as oversize.
func (p *plan) selectWithinLimit(limit int64) {
	all := make([]scannedFile, 0, len(p.added)+len(p.modified))
	p.ops = map[string]string{}
	for _, sf := range p.added {
		all = append(all, sf)
		p.ops[sf.Key] = index.OpAdded
	}
	for _, sf := range p.modified {
		all = append(all, sf)
		p.ops[sf.Key] = index.OpModified
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	var used int64
	for _, sf := range all {
		est := estimateArchived(sf)
		switch {
		case limit <= 0 || used+est <= limit:
			p.todo = append(p.todo, sf)
			p.todoBytes += sf.Size
			used += est
		case est > limit:
			p.oversize = append(p.oversize, sf)
		default:
			p.deferred = append(p.deferred, sf)
			p.deferredBytes += sf.Size
		}
	}
}

func (p *plan) count(op string) int {
	n := 0
	for _, sf := range p.todo {
		if p.ops[sf.Key] == op {
			n++
		}
	}
	return n
}

// Run executes one backup with the given configuration.
func Run(ctx context.Context, cfg config.Config, opts Options, rep Reporter) (*Result, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if len(cfg.Sources) == 0 {
		return nil, errors.New("未配置任何源目录")
	}
	dest, err := filepath.Abs(cfg.Dest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("创建目标目录: %w", err)
	}
	unlock, err := acquireLock(dest)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if !opts.DryRun {
		cleanupTemp(dest, rep)
	}
	started := now()

	prev, err := index.Latest(dest)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		rep.Log(fmt.Sprintf("基于上次备份 %s (跟踪 %d 个文件) 进行增量比对", prev.BackupID, len(prev.Files)))
	} else {
		rep.Log("未找到历史备份，执行全量备份")
	}

	rep.Progress(Progress{Phase: PhaseScanning})
	scanned, err := scan(ctx, cfg.Sources, cfg.Excludes, []string{dest}, rep.Log)
	if err != nil {
		return nil, err
	}
	rep.Log(fmt.Sprintf("扫描完成: %d 个文件", len(scanned)))

	rep.Progress(Progress{Phase: PhaseComparing, FilesTotal: len(scanned)})
	p, err := diff(ctx, prev, scanned, rep)
	if err != nil {
		return nil, err
	}
	rep.Log(fmt.Sprintf("检测到 新增 %d, 修改 %d, 删除 %d, 共 %s",
		len(p.added), len(p.modified), len(p.deleted), config.FormatSize(p.bytes)))
	limit := cfg.MaxBackupBytes()
	p.selectWithinLimit(limit)
	res := &Result{
		DryRun:        opts.DryRun,
		Added:         p.count(index.OpAdded),
		Modified:      p.count(index.OpModified),
		Deleted:       len(p.deleted),
		Bytes:         p.todoBytes,
		Deferred:      len(p.deferred),
		DeferredBytes: p.deferredBytes,
		Oversize:      len(p.oversize),
	}

	if p.empty() {
		res.NoChanges = true
		rep.Log("没有增量内容，不生成压缩包和数据库")
		return res, nil
	}
	if limit > 0 {
		rep.Log(fmt.Sprintf("单次备份上限 %s: 本次归档 %d 个文件 (%s), 延后到下次 %d 个文件 (%s)",
			config.FormatSize(limit), len(p.todo), config.FormatSize(p.todoBytes),
			len(p.deferred), config.FormatSize(p.deferredBytes)))
	}
	for i, sf := range p.oversize {
		if i == 20 {
			rep.Log(fmt.Sprintf("... 共 %d 个文件超过单次备份上限", len(p.oversize)))
			break
		}
		rep.Log(fmt.Sprintf("警告: %s (%s) 超过单次备份上限，无法备份，请调大 max_backup_size", sf.Key, config.FormatSize(sf.Size)))
	}
	if opts.DryRun {
		res.Changes = previewChanges(p)
		return res, nil
	}
	if len(p.todo) == 0 && len(p.deleted) == 0 {
		return nil, fmt.Errorf("全部 %d 个变更文件都超过单次备份上限 %s，未生成备份", len(p.oversize), config.FormatSize(limit))
	}

	lastID := ""
	if prev != nil {
		lastID = prev.BackupID
	}
	id := newBackupID(dest, started, lastID)
	tmpDir := filepath.Join(dest, tmpPrefix+id)
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		return nil, err
	}
	ix, err := archive(ctx, cfg, id, tmpDir, prev, p, rep)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}
	ix.StartedAt = started
	ix.FinishedAt = now()
	rep.Progress(Progress{Phase: PhaseFinalizing})
	if err := ix.Save(tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("写入数据库: %w", err)
	}
	finalDir := filepath.Join(dest, id)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}
	if err := ix.SaveFile(filepath.Join(dest, index.StateFileName)); err != nil {
		rep.Log("警告: 更新数据库副本 " + index.StateFileName + " 失败: " + err.Error())
	}
	res.BackupID = id
	res.Dir = finalDir
	res.Summary = &ix.Summary
	res.Added, res.Modified, res.Deleted = ix.Stats.Added, ix.Stats.Modified, ix.Stats.Deleted
	res.Bytes = ix.Stats.Bytes
	res.Deferred, res.DeferredBytes = ix.Stats.Deferred, ix.Stats.DeferredBytes
	rep.Log(fmt.Sprintf("备份完成: %s, %d 个压缩包, 共 %s", finalDir, len(ix.Volumes), config.FormatSize(ix.Stats.ArchiveBytes)))
	if ix.Stats.Deferred > 0 {
		rep.Log(fmt.Sprintf("还有 %d 个文件 (%s) 因单次上限延后，腾出空间后再次执行备份即可继续", ix.Stats.Deferred, config.FormatSize(ix.Stats.DeferredBytes)))
	}
	return res, nil
}

// diff compares the scan with the previous index. Files whose size and mtime
// are unchanged are skipped without reading them. Files with the same size
// but a new mtime are hashed and only archived if their content changed.
func diff(ctx context.Context, prev *index.Index, scanned []scannedFile, rep Reporter) (*plan, error) {
	p := &plan{scanned: scanned, touched: map[string]int64{}}
	var old map[string]index.FileEntry
	if prev != nil {
		old = prev.Files
	}
	seen := make(map[string]bool, len(scanned))
	for i, sf := range scanned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen[sf.Key] = true
		e, ok := old[sf.Key]
		switch {
		case !ok:
			p.added = append(p.added, sf)
			p.bytes += sf.Size
		case e.Size == sf.Size && e.ModTime == sf.ModTime:
			// unchanged
		case e.Size == sf.Size && e.SHA256 != "":
			sum, err := hashFile(ctx, sf.Abs)
			if err == nil && sum == e.SHA256 {
				p.touched[sf.Key] = sf.ModTime
				continue
			}
			p.modified = append(p.modified, sf)
			p.bytes += sf.Size
		default:
			p.modified = append(p.modified, sf)
			p.bytes += sf.Size
		}
		if i%5000 == 0 {
			rep.Progress(Progress{Phase: PhaseComparing, FilesTotal: len(scanned), FilesDone: i})
		}
	}
	for k := range old {
		if !seen[k] {
			p.deleted = append(p.deleted, k)
		}
	}
	sort.Strings(p.deleted)
	return p, nil
}

func archive(ctx context.Context, cfg config.Config, id, dir string, prev *index.Index, p *plan, rep Reporter) (*index.Index, error) {
	ix := index.New(id)
	if prev != nil {
		ix.PreviousID = prev.BackupID
		for k, v := range prev.Files {
			ix.Files[k] = v
		}
	}
	for _, s := range cfg.Sources {
		ix.Sources = append(ix.Sources, index.Source{Name: s.Name, Path: s.Path})
	}
	ix.Encrypted = cfg.Password != ""
	if ix.Encrypted {
		ix.Encryption = cfg.Encryption
	}
	ix.Compression = cfg.Compression
	ix.VolumeSize = cfg.VolumeBytes()
	ix.MaxSize = cfg.MaxBackupBytes()

	for k, mt := range p.touched {
		e := ix.Files[k]
		e.ModTime = mt
		ix.Files[k] = e
	}
	for _, k := range p.deleted {
		delete(ix.Files, k)
		ix.Changes = append(ix.Changes, index.Change{Path: k, Op: index.OpDeleted})
		ix.Stats.Deleted++
	}

	ix.Stats.Deferred = len(p.deferred)
	ix.Stats.DeferredBytes = p.deferredBytes
	ix.Stats.Oversize = len(p.oversize)
	for _, sf := range p.oversize {
		ix.Warnings = append(ix.Warnings, fmt.Sprintf("%s (%s) 超过单次备份上限，未备份", sf.Key, config.FormatSize(sf.Size)))
	}

	limit := cfg.MaxBackupBytes()
	vw := newVolumeWriter(dir, cfg.Prefix+"_"+id, cfg.VolumeBytes(), newEntryOptions(cfg))
	prog := Progress{Phase: PhaseArchiving, FilesTotal: len(p.todo), BytesTotal: p.todoBytes}
	var bytesDone atomic.Int64
	onBytes := func(n int64) { bytesDone.Add(n) }
	var lastReport time.Time
	report := func(force bool) {
		if force || time.Since(lastReport) > 200*time.Millisecond {
			lastReport = time.Now()
			prog.BytesDone = bytesDone.Load()
			rep.Progress(prog)
		}
	}
	// The writer streams large files itself; progress is reported from its
	// read callback so it keeps moving during long files.
	onStreamBytes := func(n int64) {
		bytesDone.Add(n)
		report(false)
	}

	opts := newEntryOptions(cfg)
	pl := startPipeline(ctx, p.todo, cfg.WorkerCount(), func(ctx context.Context, sf scannedFile) (*preparedEntry, bool, error) {
		return prepareEntry(ctx, sf, opts, onBytes)
	})
	fail := func(err error) (*index.Index, error) {
		pl.stop()
		vw.abort()
		return nil, err
	}
	for j := pl.next(); j != nil; j = pl.next() {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		sf := j.sf
		prog.Current = sf.Key
		report(true)
		var (
			vol, sum string
			n        int64
			err      error
		)
		switch {
		case j.err != nil:
			err = j.err
		case j.stream:
			// Safety net for files that grew after the scan: never exceed the limit.
			if limit > 0 && vw.written()+estimateArchived(sf) > limit {
				ix.Stats.Deferred++
				ix.Stats.DeferredBytes += sf.Size
				prog.FilesDone++
				continue
			}
			vol, n, sum, err = vw.addStream(ctx, sf, onStreamBytes)
		default:
			if limit > 0 && vw.written()+preparedSize(j.entry) > limit {
				ix.Stats.Deferred++
				ix.Stats.DeferredBytes += sf.Size
				prog.FilesDone++
				continue
			}
			n, sum = j.entry.size, j.entry.sha256
			vol, err = vw.addPrepared(j.entry)
			j.entry = nil // release the payload
		}
		prog.FilesDone++
		if err != nil {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if isWriteErr(err) {
				return fail(fmt.Errorf("写入压缩包失败: %w", err))
			}
			msg := fmt.Sprintf("%s: %v", sf.Key, err)
			rep.Log("文件读取失败，跳过 " + msg)
			ix.Errors = append(ix.Errors, msg)
			ix.Stats.Errors++
			continue
		}
		ix.Files[sf.Key] = index.FileEntry{
			Size:    n,
			ModTime: sf.ModTime,
			SHA256:  sum,
			Backup:  id,
			Volume:  vol,
		}
		op := p.ops[sf.Key]
		ix.Changes = append(ix.Changes, index.Change{Path: sf.Key, Op: op, Size: n, Volume: vol})
		if op == index.OpAdded {
			ix.Stats.Added++
		} else {
			ix.Stats.Modified++
		}
		ix.Stats.Bytes += n
	}
	pl.stop()
	if err := ctx.Err(); err != nil {
		vw.abort()
		return nil, err
	}
	report(true)
	if err := vw.close(); err != nil {
		return nil, fmt.Errorf("写入压缩包失败: %w", err)
	}
	var vols []index.Volume
	for _, v := range vw.volumes {
		vols = append(vols, index.Volume{Name: v.name, Size: v.size, Files: v.files, SHA256: v.sha256})
	}
	ix.Volumes = vols
	if ix.Stats.Added+ix.Stats.Modified+ix.Stats.Deleted == 0 {
		return nil, fmt.Errorf("没有文件归档成功 (%d 个读取错误, %d 个延后)，未生成备份", ix.Stats.Errors, ix.Stats.Deferred)
	}
	for _, v := range vols {
		ix.Stats.ArchiveBytes += v.Size
	}
	for _, e := range ix.Files {
		ix.Stats.TotalFiles++
		ix.Stats.TotalBytes += e.Size
	}
	sort.Slice(ix.Changes, func(i, j int) bool { return ix.Changes[i].Path < ix.Changes[j].Path })
	return ix, nil
}

// writeError marks failures of the destination (as opposed to the source).
type writeError struct{ err error }

func (w writeError) Error() string { return w.err.Error() }
func (w writeError) Unwrap() error { return w.err }

func isWriteErr(err error) bool {
	var we writeError
	return errors.As(err, &we)
}

func previewChanges(p *plan) []index.Change {
	var out []index.Change
	for _, sf := range p.todo {
		out = append(out, index.Change{Path: sf.Key, Op: p.ops[sf.Key], Size: sf.Size})
	}
	for _, sf := range p.deferred {
		out = append(out, index.Change{Path: sf.Key, Op: p.ops[sf.Key], Size: sf.Size, Skip: index.SkipDeferred})
	}
	for _, sf := range p.oversize {
		out = append(out, index.Change{Path: sf.Key, Op: p.ops[sf.Key], Size: sf.Size, Skip: index.SkipOversize})
	}
	for _, k := range p.deleted {
		out = append(out, index.Change{Path: k, Op: index.OpDeleted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > MaxPreviewChanges {
		out = out[:MaxPreviewChanges]
	}
	return out
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: f}, make([]byte, readBufSize)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// newBackupID derives a sortable ID from the start time and guarantees it
// is unique and newer than the latest existing backup, even if the clock
// went backwards.
func newBackupID(dest string, t time.Time, last string) string {
	base := t.Format("20060102-150405")
	if last != "" && base < last[:15] {
		base = last[:15]
	}
	exists := func(id string) bool {
		for _, p := range []string{id, tmpPrefix + id} {
			if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
				return true
			}
		}
		return false
	}
	id := base
	for n := 2; exists(id) || (last != "" && !index.LessID(last, id)); n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	return id
}

// cleanupTemp removes leftovers of interrupted runs.
func cleanupTemp(dest string, rep Reporter) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := os.RemoveAll(filepath.Join(dest, e.Name())); err == nil {
				rep.Log("清理未完成的临时目录 " + e.Name())
			}
		}
	}
}

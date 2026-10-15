package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/yeka/zip"

	"my_sync/internal/config"
	"my_sync/internal/index"
)

type nopReporter struct{ t *testing.T }

func (r nopReporter) Log(msg string)      { r.t.Log(msg) }
func (r nopReporter) Progress(p Progress) {}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Second); return c.t }

func setup(t *testing.T) (config.Config, string, string, *clock) {
	t.Helper()
	root := t.TempDir()
	srcA := filepath.Join(root, "a")
	srcB := filepath.Join(root, "b")
	dest := filepath.Join(root, "out")
	cfg := config.Default()
	cfg.Dest = dest
	cfg.Sources = []config.Source{{Name: "alpha", Path: srcA}, {Name: "beta", Path: srcB}}
	cfg.Password = "s3cret"
	cfg.VolumeSize = "300KB"
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg, srcA, srcB, &clock{t: time.Date(2026, 10, 15, 8, 0, 0, 0, time.Local)}
}

// readVolume opens one volume on its own and returns name -> content.
func readVolume(t *testing.T, path, password string) map[string][]byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer r.Close()
	out := map[string][]byte{}
	for _, f := range r.File {
		if f.IsEncrypted() {
			f.SetPassword(password)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

func run(t *testing.T, cfg config.Config, c *clock) *Result {
	t.Helper()
	res, err := Run(context.Background(), cfg, Options{Now: c.now}, nopReporter{t})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestIncrementalBackup(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	files := map[string][]byte{
		"alpha/one.txt":       []byte("hello"),
		"alpha/sub/二进制.bin":   randBytes(200 << 10),
		"alpha/sub/big.bin":   randBytes(700 << 10), // larger than a volume
		"beta/two.txt":        []byte("world"),
		"beta/deep/x/y/z.txt": bytes.Repeat([]byte("z"), 150<<10),
		"beta/skip.tmp":       []byte("excluded"),
	}
	for k, v := range files {
		base := srcA
		rel := k[len("alpha/"):]
		if k[:5] == "beta/" {
			base, rel = srcB, k[len("beta/"):]
		}
		writeFile(t, filepath.Join(base, rel), v)
	}
	delete(files, "beta/skip.tmp")

	// 1st run: full backup.
	res := run(t, cfg, clk)
	if res.NoChanges || res.Added != len(files) {
		t.Fatalf("first run: %+v", res)
	}
	if len(res.Summary.Volumes) < 3 {
		t.Fatalf("expected the backup to be split, got %d volumes", len(res.Summary.Volumes))
	}
	got := map[string][]byte{}
	for _, v := range res.Summary.Volumes {
		if v.Size > cfg.VolumeBytes() && v.Files > 1 {
			t.Errorf("volume %s exceeds limit with %d files: %d", v.Name, v.Files, v.Size)
		}
		for k, d := range readVolume(t, filepath.Join(res.Dir, v.Name), cfg.Password) {
			got[k] = d
		}
	}
	if len(got) != len(files) {
		t.Fatalf("archived %d files, want %d", len(got), len(files))
	}
	for k, v := range files {
		if !bytes.Equal(got[k], v) {
			t.Errorf("content mismatch for %s", k)
		}
	}
	if _, err := os.Stat(filepath.Join(res.Dir, index.FileName)); err != nil {
		t.Fatal("index not stored next to the archives")
	}

	// 2nd run: nothing changed -> no output at all.
	before, _ := os.ReadDir(cfg.Dest)
	res2 := run(t, cfg, clk)
	if !res2.NoChanges {
		t.Fatalf("expected no changes, got %+v", res2)
	}
	after, _ := os.ReadDir(cfg.Dest)
	if len(after) != len(before) {
		t.Fatalf("no-change run created output: %d -> %d entries", len(before), len(after))
	}

	// Touch without content change -> still no output.
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(srcB, "two.txt"), future, future)
	if res := run(t, cfg, clk); !res.NoChanges {
		t.Fatalf("touch-only should not create a backup: %+v", res)
	}

	// 3rd run: modify, add, delete.
	writeFile(t, filepath.Join(srcA, "one.txt"), []byte("hello again"))
	writeFile(t, filepath.Join(srcB, "new.txt"), []byte("new"))
	os.Remove(filepath.Join(srcB, "deep/x/y/z.txt"))
	res3 := run(t, cfg, clk)
	if res3.Added != 1 || res3.Modified != 1 || res3.Deleted != 1 {
		t.Fatalf("third run: %+v", res3)
	}
	if len(res3.Summary.Volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(res3.Summary.Volumes))
	}
	inc := readVolume(t, filepath.Join(res3.Dir, res3.Summary.Volumes[0].Name), cfg.Password)
	var names []string
	for k := range inc {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "alpha/one.txt" || names[1] != "beta/new.txt" {
		t.Fatalf("incremental archive contains %v", names)
	}

	ix, err := index.Load(res3.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if ix.PreviousID != res.BackupID {
		t.Errorf("previous id = %q, want %q", ix.PreviousID, res.BackupID)
	}
	if _, ok := ix.Files["beta/deep/x/y/z.txt"]; ok {
		t.Error("deleted file still tracked")
	}
	if e := ix.Files["beta/two.txt"]; e.Backup != res.BackupID {
		t.Errorf("unchanged file should point at first backup, got %q", e.Backup)
	}
	if e := ix.Files["alpha/one.txt"]; e.Backup != res3.BackupID {
		t.Errorf("modified file should point at latest backup, got %q", e.Backup)
	}
	if ix.Stats.TotalFiles != len(files) {
		t.Errorf("total files = %d, want %d", ix.Stats.TotalFiles, len(files))
	}
	ids, _ := index.ListIDs(cfg.Dest)
	if len(ids) != 2 {
		t.Errorf("expected 2 backups, got %v", ids)
	}

	// Restore the latest state: files come from both backups.
	files["alpha/one.txt"] = []byte("hello again")
	files["beta/new.txt"] = []byte("new")
	delete(files, "beta/deep/x/y/z.txt")
	target := t.TempDir()
	rr, err := Restore(context.Background(), RestoreOptions{Dest: cfg.Dest, Target: target, Password: cfg.Password}, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatal(err)
	}
	if rr.Files != len(files) {
		t.Fatalf("restored %d files, want %d", rr.Files, len(files))
	}
	for k, v := range files {
		data, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(k)))
		if err != nil || !bytes.Equal(data, v) {
			t.Errorf("restored %s mismatch (err=%v)", k, err)
		}
	}
	// Restoring an older state with a prefix filter.
	old := t.TempDir()
	if _, err := Restore(context.Background(), RestoreOptions{Dest: cfg.Dest, BackupID: res.BackupID, Target: old, Prefix: "alpha/one", Password: cfg.Password}, func(s string) { t.Log(s) }); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(old, "alpha/one.txt")); string(data) != "hello" {
		t.Errorf("old restore got %q", data)
	}
	if _, err := Restore(context.Background(), RestoreOptions{Dest: cfg.Dest, Target: t.TempDir(), Password: "wrong"}, func(s string) { t.Log(s) }); err == nil {
		t.Error("restore with wrong password should fail")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	writeFile(t, filepath.Join(srcA, "f.txt"), []byte("x"))
	os.MkdirAll(srcB, 0o755)
	res, err := Run(context.Background(), cfg, Options{DryRun: true, Now: clk.now}, nopReporter{t})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || len(res.Changes) != 1 {
		t.Fatalf("dry run: %+v", res)
	}
	ids, _ := index.ListIDs(cfg.Dest)
	if len(ids) != 0 {
		t.Fatalf("dry run created backups: %v", ids)
	}
}

func TestMissingSourceAborts(t *testing.T) {
	cfg, srcA, _, clk := setup(t)
	writeFile(t, filepath.Join(srcA, "f.txt"), []byte("x"))
	if _, err := Run(context.Background(), cfg, Options{Now: clk.now}, nopReporter{t}); err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestNoPasswordAndZipCrypto(t *testing.T) {
	for _, tc := range []struct{ pw, enc string }{{"", config.EncryptionAES256}, {"pw", config.EncryptionZipCrypto}} {
		cfg, srcA, srcB, clk := setup(t)
		cfg.Password, cfg.Encryption, cfg.VolumeSize = tc.pw, tc.enc, ""
		writeFile(t, filepath.Join(srcA, "f.txt"), []byte("plain"))
		os.MkdirAll(srcB, 0o755)
		res := run(t, cfg, clk)
		if len(res.Summary.Volumes) != 1 {
			t.Fatalf("volumes: %d", len(res.Summary.Volumes))
		}
		got := readVolume(t, filepath.Join(res.Dir, res.Summary.Volumes[0].Name), tc.pw)
		if string(got["alpha/f.txt"]) != "plain" {
			t.Fatalf("%+v: got %q", tc, got["alpha/f.txt"])
		}
	}
}

func TestBackupIDMonotonic(t *testing.T) {
	dest := t.TempDir()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if id := newBackupID(dest, ts, "20270101-000000"); id != "20270101-000000-2" {
		t.Fatalf("clock skew id = %s", id)
	}
	os.Mkdir(filepath.Join(dest, "20260101-000000"), 0o755)
	if id := newBackupID(dest, ts, "20260101-000000"); id != "20260101-000000-2" {
		t.Fatalf("collision id = %s", id)
	}
}

func TestExcludes(t *testing.T) {
	p := []string{"*.tmp", "node_modules", "logs/*.log", "beta/cache"}
	cases := map[[3]string]bool{
		{"alpha", "a/b.tmp", "b.tmp"}:               true,
		{"alpha", "x/node_modules", "node_modules"}: true,
		{"alpha", "logs/a.log", "a.log"}:            true,
		{"alpha", "sub/logs/a.log", "a.log"}:        false,
		{"beta", "cache", "cache"}:                  true,
		{"alpha", "cache", "cache"}:                 false,
		{"alpha", "keep.txt", "keep.txt"}:           false,
	}
	for c, want := range cases {
		if got := isExcluded(p, c[0], c[1], c[2]); got != want {
			t.Errorf("isExcluded(%v) = %v, want %v", c, got, want)
		}
	}
}

type cancelReporter struct {
	nopReporter
	cancel context.CancelFunc
}

func (r cancelReporter) Progress(p Progress) {
	if p.Phase == PhaseArchiving && p.FilesDone >= 1 {
		r.cancel()
	}
}

func TestCancelLeavesNothing(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(srcA, "f"+string(rune('0'+i))), randBytes(100<<10))
	}
	os.MkdirAll(srcB, 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Run(ctx, cfg, Options{Now: clk.now}, cancelReporter{nopReporter{t}, cancel})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	entries, _ := os.ReadDir(cfg.Dest)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("cancelled run left %s behind", e.Name())
		}
	}
}

func TestDeletionOnlyWritesIndexWithoutArchive(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	writeFile(t, filepath.Join(srcA, "keep.txt"), []byte("k"))
	writeFile(t, filepath.Join(srcB, "gone.txt"), []byte("g"))
	run(t, cfg, clk)
	os.Remove(filepath.Join(srcB, "gone.txt"))
	res := run(t, cfg, clk)
	if res.NoChanges || res.Deleted != 1 || len(res.Summary.Volumes) != 0 {
		t.Fatalf("deletion-only run: %+v", res)
	}
	entries, _ := os.ReadDir(res.Dir)
	if len(entries) != 1 || entries[0].Name() != index.FileName {
		t.Fatalf("deletion-only backup dir contains %v", entries)
	}
}

func TestMaxBackupSizeSplitsRuns(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	cfg.VolumeSize = ""
	cfg.MaxBackupSize = "350KB"
	limit := cfg.MaxBackupBytes()
	want := map[string][]byte{}
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("f%02d.bin", i)
		want["alpha/"+name] = randBytes(100 << 10)
		writeFile(t, filepath.Join(srcA, name), want["alpha/"+name])
	}
	os.MkdirAll(srcB, 0o755)

	// Each run archives what fits; the backup directory is then moved away
	// (as a user freeing disk space would) and the next run continues.
	archive := t.TempDir()
	runs, seen := 0, map[string]bool{}
	for ; runs < 10; runs++ {
		res := run(t, cfg, clk)
		if res.NoChanges {
			break
		}
		if res.Summary.Stats.ArchiveBytes > limit {
			t.Fatalf("run %d wrote %d bytes, limit %d", runs, res.Summary.Stats.ArchiveBytes, limit)
		}
		ix, _ := index.Load(res.Dir)
		for _, c := range ix.Changes {
			if seen[c.Path] {
				t.Fatalf("%s archived twice", c.Path)
			}
			seen[c.Path] = true
		}
		if left := len(want) - len(seen); res.Deferred != left {
			t.Fatalf("run %d: deferred %d, want %d", runs, res.Deferred, left)
		}
		if err := os.Rename(res.Dir, filepath.Join(archive, res.BackupID)); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 4 || len(seen) != len(want) {
		t.Fatalf("expected 4 runs covering all files, got %d runs, %d files", runs, len(seen))
	}

	// Move everything back: restoring the latest state needs the whole chain.
	entries, _ := os.ReadDir(archive)
	for _, e := range entries {
		os.Rename(filepath.Join(archive, e.Name()), filepath.Join(cfg.Dest, e.Name()))
	}
	target := t.TempDir()
	if _, err := Restore(context.Background(), RestoreOptions{Dest: cfg.Dest, Target: target, Password: cfg.Password}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if data, _ := os.ReadFile(filepath.Join(target, k)); !bytes.Equal(data, v) {
			t.Errorf("restored %s mismatch", k)
		}
	}
}

func TestMaxBackupSizeOversize(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	cfg.MaxBackupSize = "200KB"
	writeFile(t, filepath.Join(srcA, "big.bin"), randBytes(300<<10))
	writeFile(t, filepath.Join(srcB, "small.txt"), []byte("s"))

	dry, err := Run(context.Background(), cfg, Options{DryRun: true, Now: clk.now}, nopReporter{t})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Added != 1 || dry.Oversize != 1 || len(dry.Changes) != 2 || dry.Changes[0].Skip != index.SkipOversize {
		t.Fatalf("dry run: %+v", dry)
	}

	res := run(t, cfg, clk)
	if res.Added != 1 || res.Oversize != 1 || len(res.Summary.Warnings) != 1 {
		t.Fatalf("run: %+v %+v", res, res.Summary)
	}
	// Only the oversize file is left: fail loudly instead of writing nothing silently.
	if _, err := Run(context.Background(), cfg, Options{Now: clk.now}, nopReporter{t}); err == nil {
		t.Fatal("expected error when only oversize files changed")
	}
	cfg.MaxBackupSize = ""
	if res := run(t, cfg, clk); res.Added != 1 {
		t.Fatalf("after raising the limit: %+v", res)
	}
}

func TestStateCopyUsedWhenNewer(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	writeFile(t, filepath.Join(srcA, "a.txt"), []byte("a"))
	os.MkdirAll(srcB, 0o755)
	first := run(t, cfg, clk)
	if _, err := os.Stat(filepath.Join(cfg.Dest, index.StateFileName)); err != nil {
		t.Fatal("state copy not written")
	}
	os.RemoveAll(first.Dir)
	if res := run(t, cfg, clk); !res.NoChanges {
		t.Fatalf("state copy ignored: %+v", res)
	}
}

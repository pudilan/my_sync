package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yzip "github.com/yeka/zip"

	"my_sync/internal/config"
	"my_sync/internal/index"
	"my_sync/internal/zipcrypt"
)

// realMethods returns name -> effective compression method of a volume,
// looking through the AES extra field.
func realMethods(t *testing.T, path string) map[string]uint16 {
	t.Helper()
	r, err := yzip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]uint16{}
	for _, f := range r.File {
		m := f.Method
		if m == zipcrypt.MethodAES {
			for e := f.Extra; len(e) >= 4; {
				id, size := uint16(e[0])|uint16(e[1])<<8, int(e[2])|int(e[3])<<8
				if id == 0x9901 && size >= 7 {
					m = uint16(e[9]) | uint16(e[10])<<8
				}
				e = e[4+size:]
			}
		}
		out[f.Name] = m
	}
	return out
}

func mixedTree(t *testing.T, srcA string) map[string][]byte {
	files := map[string][]byte{
		"alpha/a_small.txt":     bytes.Repeat([]byte("small text "), 1000),
		"alpha/b_big_rand.bin":  randBytes(smallFileLimit + 1<<20),
		"alpha/c_small.bin":     randBytes(64 << 10),
		"alpha/d_big_text.log":  bytes.Repeat([]byte("log line with some words 12345\n"), (smallFileLimit+(2<<20))/31),
		"alpha/e_photo.jpg":     randBytes(32 << 10),
		"alpha/f_empty":         {},
		"alpha/g_中文名.txt":       []byte("你好"),
		"alpha/h_big_video.mp4": randBytes(smallFileLimit + 3<<20),
	}
	for k, v := range files {
		writeFile(t, filepath.Join(srcA, strings.TrimPrefix(k, "alpha/")), v)
	}
	return files
}

func TestStreamingAndParallelEntries(t *testing.T) {
	for _, tc := range []struct{ pw, enc string }{
		{"s3cret", config.EncryptionAES256},
		{"s3cret", config.EncryptionZipCrypto},
		{"", config.EncryptionAES256},
	} {
		t.Run(tc.enc+"-"+tc.pw, func(t *testing.T) {
			cfg, srcA, srcB, clk := setup(t)
			cfg.Password, cfg.Encryption, cfg.VolumeSize = tc.pw, tc.enc, "12MB"
			os.MkdirAll(srcB, 0o755)
			files := mixedTree(t, srcA)
			res := run(t, cfg, clk)
			if res.Added != len(files) {
				t.Fatalf("added %d, want %d", res.Added, len(files))
			}
			got := map[string][]byte{}
			methods := map[string]uint16{}
			var order []string
			for _, v := range res.Summary.Volumes {
				path := filepath.Join(res.Dir, v.Name)
				for k, d := range readVolume(t, path, tc.pw) {
					got[k] = d
				}
				for k, m := range realMethods(t, path) {
					methods[k] = m
				}
				r, _ := yzip.OpenReader(path)
				for _, f := range r.File {
					order = append(order, f.Name)
				}
				r.Close()
				if v.Size > cfg.VolumeBytes() && v.Files > 1 {
					t.Errorf("volume %s exceeds limit with %d files: %d", v.Name, v.Files, v.Size)
				}
			}
			for k, v := range files {
				if !bytes.Equal(got[k], v) {
					t.Errorf("%s: content mismatch", k)
				}
			}
			for i := 1; i < len(order); i++ {
				if order[i-1] > order[i] {
					t.Errorf("entries out of order: %v", order)
					break
				}
			}
			// auto: known formats and incompressible small files are stored.
			want := map[string]uint16{
				"alpha/a_small.txt": zip.Deflate, "alpha/d_big_text.log": zip.Deflate,
				"alpha/e_photo.jpg": zip.Store, "alpha/h_big_video.mp4": zip.Store, "alpha/c_small.bin": zip.Store,
			}
			for k, m := range want {
				if methods[k] != m {
					t.Errorf("%s: method %d, want %d", k, methods[k], m)
				}
			}
			ix, _ := index.Load(res.Dir)
			for k, v := range files {
				if ix.Files[k].Size != int64(len(v)) {
					t.Errorf("%s: indexed size %d", k, ix.Files[k].Size)
				}
			}
			// Unencrypted archives must also open with the standard library.
			if tc.pw == "" {
				for _, v := range res.Summary.Volumes {
					r, err := zip.OpenReader(filepath.Join(res.Dir, v.Name))
					if err != nil {
						t.Fatal(err)
					}
					for _, f := range r.File {
						rc, err := f.Open()
						if err != nil {
							t.Fatal(err)
						}
						var b bytes.Buffer
						if _, err := b.ReadFrom(rc); err != nil {
							t.Errorf("std reader %s: %v", f.Name, err)
						}
						rc.Close()
					}
					r.Close()
				}
			}
		})
	}
}

func TestWorkerCountDoesNotChangeResult(t *testing.T) {
	var indexes []*index.Index
	for _, workers := range []int{1, 8} {
		cfg, srcA, srcB, clk := setup(t)
		cfg.Workers = workers
		os.MkdirAll(srcB, 0o755)
		for i := 0; i < 200; i++ {
			writeFile(t, filepath.Join(srcA, "d", strings.Repeat("x", i%7)+string(rune('a'+i%26))+".txt"), bytes.Repeat([]byte{byte(i)}, i*97))
		}
		res := run(t, cfg, clk)
		ix, _ := index.Load(res.Dir)
		indexes = append(indexes, ix)
	}
	a, b := indexes[0], indexes[1]
	if len(a.Files) != len(b.Files) {
		t.Fatalf("file count differs: %d vs %d", len(a.Files), len(b.Files))
	}
	for k, e := range a.Files {
		if b.Files[k].SHA256 != e.SHA256 || b.Files[k].Volume[len(b.Files[k].Volume)-7:] != e.Volume[len(e.Volume)-7:] {
			t.Errorf("%s differs between worker counts", k)
		}
	}
}

func TestMaxBackupSizeWithStreaming(t *testing.T) {
	cfg, srcA, srcB, clk := setup(t)
	cfg.VolumeSize = ""
	cfg.Compression = config.CompressionStore
	cfg.MaxBackupSize = "20MB"
	os.MkdirAll(srcB, 0o755)
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, filepath.Join(srcA, n+".bin"), randBytes(smallFileLimit+1<<20))
	}
	res := run(t, cfg, clk)
	if res.Added != 2 || res.Deferred != 1 {
		t.Fatalf("added %d deferred %d", res.Added, res.Deferred)
	}
	if res.Summary.Stats.ArchiveBytes > cfg.MaxBackupBytes() {
		t.Fatalf("wrote %d > limit %d", res.Summary.Stats.ArchiveBytes, cfg.MaxBackupBytes())
	}
}

func TestCompressionModes(t *testing.T) {
	text := bytes.Repeat([]byte("compressible "), 4000)
	for _, tc := range []struct {
		mode string
		want map[string]uint16
	}{
		{config.CompressionStore, map[string]uint16{"alpha/t.txt": zip.Store, "alpha/p.png": zip.Store}},
		{config.CompressionDeflate, map[string]uint16{"alpha/t.txt": zip.Deflate, "alpha/p.png": zip.Deflate}},
		{config.CompressionAuto, map[string]uint16{"alpha/t.txt": zip.Deflate, "alpha/p.png": zip.Store}},
	} {
		cfg, srcA, srcB, clk := setup(t)
		cfg.Compression = tc.mode
		os.MkdirAll(srcB, 0o755)
		writeFile(t, filepath.Join(srcA, "t.txt"), text)
		writeFile(t, filepath.Join(srcA, "p.png"), text) // compressible, but auto trusts the extension
		res := run(t, cfg, clk)
		got := realMethods(t, filepath.Join(res.Dir, res.Summary.Volumes[0].Name))
		for k, m := range tc.want {
			if got[k] != m {
				t.Errorf("%s %s: method %d, want %d", tc.mode, k, got[k], m)
			}
		}
	}
}

func TestPrepareEntryFileGrowsFallsBackToStream(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, randBytes(4*smallFileLimit+10), 0o644)
	_, tooBig, err := prepareEntry(context.Background(), scannedFile{Key: "f", Abs: p, Size: 10}, entryOptions{level: 5}, nil)
	if err != nil || !tooBig {
		t.Fatalf("tooBig=%v err=%v", tooBig, err)
	}
}

package backup

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"my_sync/internal/config"
)

// scannedFile is a regular file found in a source directory.
type scannedFile struct {
	Key     string // "<source name>/<relative path>" with forward slashes
	Abs     string
	Size    int64
	ModTime int64 // unix nanoseconds
	Mode    os.FileMode
}

// scan walks all sources and returns their regular files. A source root that
// is missing or unreadable aborts the scan: treating it as empty would mark
// every file of that source as deleted.
func scan(ctx context.Context, sources []config.Source, excludes []string, skipDirs []string, warn func(string)) ([]scannedFile, error) {
	skip := map[string]bool{}
	for _, d := range skipDirs {
		skip[d] = true
	}
	var files []scannedFile
	for _, src := range sources {
		root, err := filepath.Abs(src.Path)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("源目录 %s (%s) 不可访问: %w", src.Name, src.Path, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("源 %s (%s) 不是目录", src.Name, src.Path)
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				if p == root {
					return err
				}
				warn(fmt.Sprintf("跳过 %s: %v", p, err))
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if p == root {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if skip[p] || isExcluded(excludes, src.Name, rel, d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || isExcluded(excludes, src.Name, rel, d.Name()) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				warn(fmt.Sprintf("跳过 %s: %v", p, err))
				return nil
			}
			files = append(files, scannedFile{
				Key:     src.Name + "/" + rel,
				Abs:     p,
				Size:    info.Size(),
				ModTime: info.ModTime().UnixNano(),
				Mode:    info.Mode(),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("扫描 %s: %w", src.Path, err)
		}
	}
	return files, nil
}

// isExcluded matches a file or directory against the exclude patterns.
// Patterns without "/" match any path element name (e.g. "*.tmp",
// "node_modules"); patterns with "/" match the path relative to the source
// root, optionally prefixed with the source name (e.g. "logs/*.log",
// "docs/cache").
func isExcluded(patterns []string, source, rel, name string) bool {
	for _, p := range patterns {
		if strings.Contains(p, "/") {
			p = strings.TrimPrefix(p, "/")
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			if ok, _ := path.Match(p, source+"/"+rel); ok {
				return true
			}
		} else if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

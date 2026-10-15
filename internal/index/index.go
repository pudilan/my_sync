// Package index implements the backup database.
//
// Every backup directory contains one index.json. It is a complete snapshot:
// besides describing what this backup added, it records the state of every
// tracked file and in which backup/volume its latest version lives. An
// incremental run therefore only needs the newest index and never has to open
// earlier archives.
package index

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// FileName is the name of the database file inside a backup directory.
const FileName = "index.json"

// StateFileName is a copy of the newest index kept in the destination root.
// It keeps incremental backups working after backup directories have been
// moved elsewhere, e.g. to free disk space between runs.
const StateFileName = "index.latest.json"

// Format identifies the index schema version.
const Format = "my_sync/index/v1"

// Change operations.
const (
	OpAdded    = "added"
	OpModified = "modified"
	OpDeleted  = "deleted"
)

// FileEntry is the recorded state of one backed up file.
type FileEntry struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // unix nanoseconds
	SHA256  string `json:"sha256"`
	Backup  string `json:"backup"` // backup ID that holds the latest version
	Volume  string `json:"volume"` // archive file inside that backup directory
}

// Volume describes one archive file of a backup.
type Volume struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Files  int    `json:"files"`
	SHA256 string `json:"sha256"`
}

// Reasons a detected change was not archived (dry run previews only).
const (
	SkipDeferred = "deferred" // postponed to the next run by max_backup_size
	SkipOversize = "oversize" // larger than max_backup_size on its own
)

// Change is one entry of the change log of a backup.
type Change struct {
	Path   string `json:"path"`
	Op     string `json:"op"`
	Size   int64  `json:"size,omitempty"`
	Volume string `json:"volume,omitempty"`
	Skip   string `json:"skip,omitempty"`
}

// Source mirrors config.Source to keep this package dependency free.
type Source struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Stats summarises a backup.
type Stats struct {
	Added        int   `json:"added"`
	Modified     int   `json:"modified"`
	Deleted      int   `json:"deleted"`
	Errors       int   `json:"errors"`
	Bytes        int64 `json:"bytes"`         // original size of archived files
	ArchiveBytes int64 `json:"archive_bytes"` // total size of volumes
	TotalFiles   int   `json:"total_files"`   // tracked files after this backup
	TotalBytes   int64 `json:"total_bytes"`
	// Changed files left for the next run because of max_backup_size.
	Deferred      int   `json:"deferred,omitempty"`
	DeferredBytes int64 `json:"deferred_bytes,omitempty"`
	// Changed files larger than max_backup_size, which can never be archived.
	Oversize int `json:"oversize,omitempty"`
}

// Summary is the metadata part of an index (everything but Files).
type Summary struct {
	Format      string    `json:"format"`
	BackupID    string    `json:"backup_id"`
	PreviousID  string    `json:"previous_id,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Sources     []Source  `json:"sources"`
	Encrypted   bool      `json:"encrypted"`
	Encryption  string    `json:"encryption,omitempty"`
	Compression string    `json:"compression"`
	VolumeSize  int64     `json:"volume_size"`
	MaxSize     int64     `json:"max_backup_size,omitempty"`
	Volumes     []Volume  `json:"volumes"`
	Stats       Stats     `json:"stats"`
	Errors      []string  `json:"errors,omitempty"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// Index is the full database of one backup.
type Index struct {
	Summary
	Changes []Change             `json:"changes"`
	Files   map[string]FileEntry `json:"files"`
}

// New creates an empty index.
func New(id string) *Index {
	return &Index{
		Summary: Summary{Format: Format, BackupID: id},
		Files:   map[string]FileEntry{},
	}
}

// Save writes the index atomically into dir.
func (ix *Index) Save(dir string) error {
	return ix.SaveFile(filepath.Join(dir, FileName))
}

// SaveFile writes the index atomically to path.
func (ix *Index) SaveFile(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	err = enc.Encode(ix)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads the full index from a backup directory.
func Load(dir string) (*Index, error) {
	return LoadFile(filepath.Join(dir, FileName))
}

// LoadFile reads the full index from a file.
func LoadFile(path string) (*Index, error) {
	ix := &Index{}
	if err := readJSON(path, ix); err != nil {
		return nil, err
	}
	if ix.Files == nil {
		ix.Files = map[string]FileEntry{}
	}
	return ix, nil
}

// LoadSummary reads only the metadata of an index.
func LoadSummary(dir string) (*Summary, error) {
	s := &Summary{}
	if err := readJSON(filepath.Join(dir, FileName), s); err != nil {
		return nil, err
	}
	return s, nil
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(bufio.NewReaderSize(f, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("读取 %s: %w", path, err)
	}
	return nil
}

// idRe matches backup directory names, e.g. 20261015-073012 or 20261015-073012-2.
var idRe = regexp.MustCompile(`^\d{8}-\d{6}(-\d+)?$`)

// IsBackupID reports whether name looks like a backup directory name.
func IsBackupID(name string) bool { return idRe.MatchString(name) }

// ListIDs returns the IDs of all completed backups in dest, oldest first.
// A directory only counts as a backup once it contains an index file.
func ListIDs(dest string) ([]string, error) {
	entries, err := os.ReadDir(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || !IsBackupID(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dest, e.Name(), FileName)); err == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Slice(ids, func(i, j int) bool { return LessID(ids[i], ids[j]) })
	return ids, nil
}

// LessID orders backup IDs chronologically, treating "-N" suffixes numerically.
func LessID(a, b string) bool {
	if a[:15] != b[:15] {
		return a[:15] < b[:15]
	}
	var na, nb int
	if len(a) > 16 {
		fmt.Sscan(a[16:], &na)
	}
	if len(b) > 16 {
		fmt.Sscan(b[16:], &nb)
	}
	return na < nb
}

// Latest loads the newest index in dest: the one of the newest backup
// directory or the state copy in the root, whichever is newer. It returns
// nil if there is neither.
func Latest(dest string) (*Index, error) {
	ids, err := ListIDs(dest)
	if err != nil {
		return nil, err
	}
	var ix *Index
	if len(ids) > 0 {
		last := ids[len(ids)-1]
		if ix, err = Load(filepath.Join(dest, last)); err != nil {
			// A corrupt index must not silently trigger a full backup.
			return nil, fmt.Errorf("最新备份 %s 的数据库不可读: %w", last, err)
		}
	}
	statePath := filepath.Join(dest, StateFileName)
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		return ix, nil
	}
	st, err := LoadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("数据库副本 %s 不可读: %w", statePath, err)
	}
	if ix == nil || (IsBackupID(st.BackupID) && LessID(ix.BackupID, st.BackupID)) {
		return st, nil
	}
	return ix, nil
}

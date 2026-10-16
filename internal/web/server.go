// Package web serves the management UI and its JSON API.
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"my_sync/internal/config"
	"my_sync/internal/index"
	"my_sync/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const pageSize = 200

// Server is the HTTP handler of the management UI.
type Server struct {
	svc   *service.Service
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// New builds the UI server.
func New(svc *service.Service) (*Server, error) {
	s := &Server{svc: svc, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	funcs := template.FuncMap{
		"size":    config.FormatSize,
		"ftime":   formatTime,
		"op":      opLabel,
		"opClass": func(op string) string { return "op-" + op },
		"encLabel": func(enc string) string {
			switch enc {
			case config.EncryptionAES256:
				return "AES-256"
			case config.EncryptionZipCrypto:
				return "ZipCrypto"
			}
			return enc
		},
		"dur": func(a, b time.Time) string {
			if a.IsZero() || b.IsZero() {
				return "-"
			}
			return b.Sub(a).Round(time.Second).String()
		},
		"join": strings.Join,
	}
	for _, name := range []string{"dashboard", "backup", "files", "config"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /backups/{id}", s.handleBackup)
	s.mux.HandleFunc("GET /files", s.handleFiles)
	s.mux.HandleFunc("GET /config", s.handleConfig)
	s.mux.HandleFunc("POST /config", s.handleConfigSave)
	s.mux.HandleFunc("GET /download/{id}/{name}", s.handleDownload)
	s.mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	s.mux.HandleFunc("GET /api/backups", s.handleAPIBackups)
	s.mux.HandleFunc("POST /api/run", s.handleAPIRun)
	s.mux.HandleFunc("POST /api/cancel", s.handleAPICancel)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" && !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="my_sync", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	user, password, enabled := config.UICredentials()
	if !enabled {
		return true
	}
	u, p, ok := r.BasicAuth()
	return ok &&
		subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(password)) == 1
}

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	data["Page"] = page
	data["CPUs"] = runtime.NumCPU()
	data["Running"] = s.svc.Running()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].Execute(w, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.svc.Config()
	backups, err := s.svc.Backups()
	data := map[string]any{
		"Cfg":       cfg,
		"VolumeLbl": volumeLabel(cfg),
		"MaxLbl":    maxLabel(cfg),
		"Backups":   backups,
		"Status":    s.svc.Status(),
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, "dashboard", data)
}

type pager struct {
	Page, Pages, Total int
	Prev, Next         string
}

func paginate(r *http.Request, total int) (pager, int, int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pages := (total + pageSize - 1) / pageSize
	if pages == 0 {
		pages = 1
	}
	page = min(max(page, 1), pages)
	link := func(p int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p))
		return r.URL.Path + "?" + q.Encode()
	}
	pg := pager{Page: page, Pages: pages, Total: total}
	if page > 1 {
		pg.Prev = link(page - 1)
	}
	if page < pages {
		pg.Next = link(page + 1)
	}
	from := (page - 1) * pageSize
	return pg, from, min(from+pageSize, total)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	ix, err := s.svc.Index(r.PathValue("id"))
	if err != nil {
		s.notFound(w, err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	opFilter := r.URL.Query().Get("op")
	counts := map[string]int{}
	var changes []index.Change
	for _, c := range ix.Changes {
		counts[c.Op]++
		if opFilter != "" && c.Op != opFilter {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Path), q) {
			continue
		}
		changes = append(changes, c)
	}
	pg, from, to := paginate(r, len(changes))
	s.render(w, "backup", map[string]any{
		"Ix":      ix,
		"Changes": changes[from:to],
		"Pager":   pg,
		"Q":       r.URL.Query().Get("q"),
		"Op":      opFilter,
		"Counts":  counts,
	})
}

type fileRow struct {
	Path string
	index.FileEntry
	MTime time.Time
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Q": r.URL.Query().Get("q")}
	ix, err := s.svc.LatestIndex()
	if err != nil {
		data["Error"] = err.Error()
	}
	var rows []fileRow
	if ix != nil {
		q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		for p, e := range ix.Files {
			if q == "" || strings.Contains(strings.ToLower(p), q) {
				rows = append(rows, fileRow{Path: p, FileEntry: e, MTime: time.Unix(0, e.ModTime)})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
		data["Ix"] = ix
	}
	pg, from, to := paginate(r, len(rows))
	data["Rows"] = rows[from:to]
	data["Pager"] = pg
	s.render(w, "files", data)
}

// configForm is the editable representation of the config in the UI.
type configForm struct {
	Sources, Excludes                       string
	Dest, Prefix, VolumeSize, MaxBackupSize string
	Encryption, Compression, Schedule       string
	Listen                                  string
	CompressionLevel, Workers               int
	HasPassword, UIAuth                     bool
}

func formFromConfig(c config.Config) configForm {
	var src []string
	for _, s := range c.Sources {
		src = append(src, s.Name+" = "+s.Path)
	}
	_, _, uiAuth := config.UICredentials()
	return configForm{
		Sources:          strings.Join(src, "\n"),
		Excludes:         strings.Join(c.Excludes, "\n"),
		Dest:             c.Dest,
		Prefix:           c.Prefix,
		VolumeSize:       c.VolumeSize,
		MaxBackupSize:    c.MaxBackupSize,
		Encryption:       c.Encryption,
		Compression:      c.Compression,
		CompressionLevel: c.CompressionLevel,
		Workers:          c.Workers,
		Schedule:         c.Schedule,
		Listen:           c.Listen,
		HasPassword:      c.Password != "",
		UIAuth:           uiAuth,
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Form":       formFromConfig(s.svc.Store().File()),
		"Effective":  s.svc.Config(),
		"ConfigPath": s.svc.Store().Path(),
	}
	if r.URL.Query().Get("saved") == "1" {
		data["Saved"] = true
	}
	s.render(w, "config", data)
}

func parseSources(text string) []config.Source {
	var out []config.Source
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var src config.Source
		if name, p, ok := strings.Cut(line, "="); ok {
			src = config.Source{Name: strings.TrimSpace(name), Path: strings.TrimSpace(p)}
		} else {
			src = config.Source{Path: line}
		}
		out = append(out, src)
	}
	return out
}

func splitLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	old := s.svc.Store().File()
	c := old.Clone()
	c.Sources = parseSources(r.FormValue("sources"))
	c.Excludes = splitLines(r.FormValue("excludes"))
	c.Dest = r.FormValue("dest")
	c.Prefix = r.FormValue("prefix")
	c.VolumeSize = strings.TrimSpace(r.FormValue("volume_size"))
	c.MaxBackupSize = strings.TrimSpace(r.FormValue("max_backup_size"))
	c.Encryption = r.FormValue("encryption")
	c.Compression = r.FormValue("compression")
	c.CompressionLevel, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("compression_level")))
	c.Workers, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("workers")))
	c.Schedule = strings.TrimSpace(r.FormValue("schedule"))
	if pw := r.FormValue("password"); pw != "" {
		c.Password = pw
	} else if r.FormValue("clear_password") == "1" {
		c.Password = ""
	}
	if err := s.svc.UpdateConfig(c); err != nil {
		form := formFromConfig(c)
		form.HasPassword = c.Password != ""
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "config", map[string]any{
			"Form":       form,
			"Effective":  s.svc.Config(),
			"ConfigPath": s.svc.Store().Path(),
			"Error":      err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/config?saved=1", http.StatusSeeOther)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !index.IsBackupID(id) {
		s.notFound(w, errors.New("非法备份 ID"))
		return
	}
	dir := filepath.Join(s.svc.Config().Dest, id)
	allowed := name == index.FileName
	if !allowed {
		sum, err := index.LoadSummary(dir)
		if err != nil {
			s.notFound(w, err)
			return
		}
		for _, v := range sum.Volumes {
			allowed = allowed || v.Name == name
		}
	}
	if !allowed {
		s.notFound(w, fmt.Errorf("文件 %s 不属于备份 %s", name, id))
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeFile(w, r, filepath.Join(dir, name))
}

func (s *Server) notFound(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Status())
}

func (s *Server) handleAPIBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.svc.Backups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, backups)
}

func (s *Server) handleAPIRun(w http.ResponseWriter, r *http.Request) {
	dry := r.FormValue("dry_run") == "1" || r.FormValue("dry_run") == "true"
	seq, err := s.svc.Trigger(service.TriggerManual, dry)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"seq": seq})
}

func (s *Server) handleAPICancel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": s.svc.Cancel()})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func opLabel(op string) string {
	switch op {
	case index.OpAdded:
		return "新增"
	case index.OpModified:
		return "修改"
	case index.OpDeleted:
		return "删除"
	}
	return op
}

func maxLabel(c config.Config) string {
	if n := c.MaxBackupBytes(); n > 0 {
		return config.FormatSize(n)
	}
	return "不限制"
}

func volumeLabel(c config.Config) string {
	if n := c.VolumeBytes(); n > 0 {
		return config.FormatSize(n)
	}
	return "不拆分"
}

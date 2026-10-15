package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"my_sync/internal/config"
	"my_sync/internal/service"
)

func newTestServer(t *testing.T) (*Server, *service.Service, config.Config) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644)
	store, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := store.File()
	c.Dest = filepath.Join(dir, "out")
	c.Sources = []config.Source{{Name: "src", Path: src}}
	c.UI = config.UIConfig{Username: "u", Password: "p"}
	if err := store.Update(c); err != nil {
		t.Fatal(err)
	}
	svc := service.New(store)
	s, err := New(svc)
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, store.Get()
}

func do(s http.Handler, method, path string, auth bool, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth {
		req.SetBasicAuth("u", "p")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestServer(t *testing.T) {
	s, svc, _ := newTestServer(t)
	if c := do(s, "GET", "/", false, nil).Code; c != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", c)
	}
	if c := do(s, "GET", "/healthz", false, nil).Code; c != http.StatusOK {
		t.Fatalf("healthz: %d", c)
	}
	if c := do(s, "POST", "/api/run", true, nil).Code; c != http.StatusAccepted {
		t.Fatalf("run: %d", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for svc.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	st := svc.Status()
	if st.Current == nil || st.Current.Result == nil || st.Current.Result.BackupID == "" {
		t.Fatalf("run result: %+v", st.Current)
	}
	id := st.Current.Result.BackupID
	vol := st.Current.Result.Summary.Volumes[0].Name
	for _, p := range []string{"/", "/files", "/files?q=a", "/config", "/backups/" + id, "/api/status", "/api/backups"} {
		if rec := do(s, "GET", p, true, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if rec := do(s, "GET", "/backups/"+id, true, nil); !strings.Contains(rec.Body.String(), "src/a.txt") {
		t.Error("backup page does not list the change")
	}
	if c := do(s, "GET", "/download/"+id+"/"+vol, true, nil).Code; c != http.StatusOK {
		t.Errorf("download volume: %d", c)
	}
	for _, p := range []string{"/download/" + id + "/..%2F..%2Fconfig.yaml", "/download/x/index.json", "/backups/..%2F"} {
		if c := do(s, "GET", p, true, nil).Code; c != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, c)
		}
	}
}

func TestCrossOriginPostAllowed(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, origin := range []string{"http://localhost:3000", "null"} {
		if c := do(s, "POST", "/api/cancel", true, map[string]string{"Origin": origin}).Code; c != http.StatusOK {
			t.Errorf("Origin %s: %d", origin, c)
		}
	}
}

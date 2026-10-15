package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"": 0, "0": 0, "1024": 1024, "1KB": 1 << 10, "500MB": 500 << 20,
		"2G": 2 << 30, "1.5GB": 3 << 29, "4GiB": 4 << 30, " 10 mb ": 10 << 20,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "10XB", "-1MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestSchedule(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.Local)
	s, err := ParseSchedule("02:30")
	if err != nil || !s.Daily {
		t.Fatal(s, err)
	}
	if next := s.Next(now); !next.Equal(time.Date(2026, 10, 16, 2, 30, 0, 0, time.Local)) {
		t.Errorf("daily next = %v", next)
	}
	s, _ = ParseSchedule("daily 04:00")
	if next := s.Next(now); !next.Equal(time.Date(2026, 10, 15, 4, 0, 0, 0, time.Local)) {
		t.Errorf("daily next = %v", next)
	}
	s, _ = ParseSchedule("@every 6h")
	if s.Every != 6*time.Hour {
		t.Errorf("every = %v", s.Every)
	}
	if s, _ := ParseSchedule(""); s.Enabled() {
		t.Error("empty schedule should be disabled")
	}
	for _, bad := range []string{"25:00", "10s", "soon"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q) should fail", bad)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "config.yaml")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("default config not created")
	}
	c := s.File()
	c.Sources = []Source{{Path: "/data/docs"}, {Name: "pics", Path: "/data/photos/"}}
	c.Password = "pw"
	if err := s.Update(c); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get()
	if got.Sources[0].Name != "docs" || got.Sources[1].Path != "/data/photos" || got.Password != "pw" {
		t.Errorf("round trip: %+v", got)
	}
	t.Setenv(EnvPassword, "fromenv")
	s3, _ := Load(path)
	if s3.Get().Password != "fromenv" || s3.File().Password != "pw" {
		t.Error("env override not applied correctly")
	}
	c.Sources = append(c.Sources, Source{Name: "docs", Path: "/other"})
	if err := s.Update(c); err == nil {
		t.Error("duplicate source names should be rejected")
	}
}

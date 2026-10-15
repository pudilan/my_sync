// Package service coordinates backup runs for the web UI and the scheduler:
// it guarantees a single run at a time, keeps progress and logs, and caches
// backup indexes for browsing.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"my_sync/internal/backup"
	"my_sync/internal/config"
	"my_sync/internal/index"
)

// ErrBusy is returned when a backup is already running.
var ErrBusy = errors.New("已有备份任务正在运行")

// Triggers of a run.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
)

const (
	maxLogLines   = 1000
	maxHistory    = 20
	maxIndexCache = 3
)

// LogLine is one timestamped log message of a run.
type LogLine struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"msg"`
}

// Run describes a backup run, current or finished.
type Run struct {
	Seq        int64           `json:"seq"`
	Trigger    string          `json:"trigger"`
	DryRun     bool            `json:"dry_run"`
	Running    bool            `json:"running"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at,omitempty"`
	Progress   backup.Progress `json:"progress"`
	Result     *backup.Result  `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Logs       []LogLine       `json:"logs,omitempty"`
}

// Status is a snapshot for the UI.
type Status struct {
	Current  *Run      `json:"current,omitempty"`
	History  []Run     `json:"history"`
	NextRun  time.Time `json:"next_run,omitempty"`
	Schedule string    `json:"schedule"`
}

// Service owns run state and caches.
type Service struct {
	store *config.Store

	mu      sync.Mutex
	seq     int64
	current *Run
	history []Run // newest first, logs stripped
	cancel  context.CancelFunc
	nextRun time.Time

	reschedule chan struct{}

	cacheMu   sync.Mutex
	summaries map[string]*index.Summary
	indexes   map[string]*index.Index
	indexLRU  []string
}

// New creates a service bound to a config store.
func New(store *config.Store) *Service {
	return &Service{
		store:      store,
		reschedule: make(chan struct{}, 1),
		summaries:  map[string]*index.Summary{},
		indexes:    map[string]*index.Index{},
	}
}

// Config returns the effective configuration.
func (s *Service) Config() config.Config { return s.store.Get() }

// Store exposes the underlying config store.
func (s *Service) Store() *config.Store { return s.store }

// UpdateConfig persists a new configuration and reloads the scheduler.
func (s *Service) UpdateConfig(c config.Config) error {
	if err := s.store.Update(c); err != nil {
		return err
	}
	select {
	case s.reschedule <- struct{}{}:
	default:
	}
	return nil
}

// Start runs the scheduler until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	go s.scheduler(ctx)
}

func (s *Service) scheduler(ctx context.Context) {
	for {
		sched, _ := config.ParseSchedule(s.store.Get().Schedule)
		var timer <-chan time.Time
		next := time.Time{}
		if sched.Enabled() {
			next = sched.Next(time.Now())
			timer = time.After(time.Until(next))
		}
		s.mu.Lock()
		s.nextRun = next
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.Cancel()
			return
		case <-s.reschedule:
		case <-timer:
			if _, err := s.Trigger(TriggerSchedule, false); err != nil {
				log.Printf("定时备份未执行: %v", err)
			}
		}
	}
}

// Trigger launches a backup in the background and returns its sequence number.
func (s *Service) Trigger(trigger string, dryRun bool) (int64, error) {
	s.mu.Lock()
	if s.current != nil && s.current.Running {
		s.mu.Unlock()
		return 0, ErrBusy
	}
	s.seq++
	run := &Run{Seq: s.seq, Trigger: trigger, DryRun: dryRun, Running: true, StartedAt: time.Now()}
	if s.current != nil {
		s.pushHistory(*s.current)
	}
	s.current = run
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()

	cfg := s.store.Get()
	go func() {
		defer cancel()
		rep := &reporter{s: s, run: run}
		kind := "备份"
		if dryRun {
			kind = "预览"
		}
		rep.Log(fmt.Sprintf("开始%s (触发方式: %s)", kind, trigger))
		res, err := backup.Run(ctx, cfg, backup.Options{DryRun: dryRun}, rep)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				err = errors.New("任务已取消")
			}
			rep.Log("失败: " + err.Error())
		}
		s.mu.Lock()
		run.Running = false
		run.FinishedAt = time.Now()
		run.Result = res
		if err != nil {
			run.Error = err.Error()
		}
		s.cancel = nil
		s.mu.Unlock()
	}()
	return run.Seq, nil
}

// Cancel stops the running backup, if any.
func (s *Service) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

func (s *Service) pushHistory(r Run) {
	r.Logs = nil
	if r.Result != nil {
		res := *r.Result
		res.Changes = nil
		r.Result = &res
	}
	s.history = append([]Run{r}, s.history...)
	if len(s.history) > maxHistory {
		s.history = s.history[:maxHistory]
	}
}

// Status returns a deep enough copy of the run state for rendering.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{History: append([]Run(nil), s.history...), NextRun: s.nextRun, Schedule: s.store.Get().Schedule}
	if s.current != nil {
		c := *s.current
		c.Logs = append([]LogLine(nil), s.current.Logs...)
		st.Current = &c
	}
	return st
}

// Running reports whether a backup is in progress.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current != nil && s.current.Running
}

type reporter struct {
	s   *Service
	run *Run
}

func (r *reporter) Log(msg string) {
	log.Printf("[run %d] %s", r.run.Seq, msg)
	r.s.mu.Lock()
	r.run.Logs = append(r.run.Logs, LogLine{Time: time.Now(), Msg: msg})
	if n := len(r.run.Logs); n > maxLogLines {
		r.run.Logs = append(r.run.Logs[:0:0], r.run.Logs[n-maxLogLines:]...)
	}
	r.s.mu.Unlock()
}

func (r *reporter) Progress(p backup.Progress) {
	r.s.mu.Lock()
	r.run.Progress = p
	r.s.mu.Unlock()
}

// Backups lists the summaries of all backups in the destination, newest first.
func (s *Service) Backups() ([]*index.Summary, error) {
	dest := s.store.Get().Dest
	ids, err := index.ListIDs(dest)
	if err != nil {
		return nil, err
	}
	out := make([]*index.Summary, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		sum, err := s.summary(dest, ids[i])
		if err != nil {
			log.Printf("读取备份 %s 失败: %v", ids[i], err)
			continue
		}
		out = append(out, sum)
	}
	return out, nil
}

func (s *Service) summary(dest, id string) (*index.Summary, error) {
	key := dest + "|" + id
	s.cacheMu.Lock()
	sum, ok := s.summaries[key]
	s.cacheMu.Unlock()
	if ok {
		return sum, nil
	}
	sum, err := index.LoadSummary(filepath.Join(dest, id))
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.summaries[key] = sum
	s.cacheMu.Unlock()
	return sum, nil
}

// Index loads the full index of a backup (cached, indexes are immutable).
func (s *Service) Index(id string) (*index.Index, error) {
	if !index.IsBackupID(id) {
		return nil, fmt.Errorf("非法备份 ID: %q", id)
	}
	dest := s.store.Get().Dest
	key := dest + "|" + id
	s.cacheMu.Lock()
	if ix, ok := s.indexes[key]; ok {
		s.cacheMu.Unlock()
		return ix, nil
	}
	s.cacheMu.Unlock()
	ix, err := index.Load(filepath.Join(dest, id))
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.indexes[key] = ix
	s.indexLRU = append(s.indexLRU, key)
	if len(s.indexLRU) > maxIndexCache {
		delete(s.indexes, s.indexLRU[0])
		s.indexLRU = s.indexLRU[1:]
	}
	return ix, nil
}

// LatestIndex returns the newest index, or nil if there are no backups.
func (s *Service) LatestIndex() (*index.Index, error) {
	ids, err := index.ListIDs(s.store.Get().Dest)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return s.Index(ids[len(ids)-1])
}

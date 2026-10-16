package backup

import (
	"context"
	"sync"
)

// job is one file on its way into the archive. Small files are encoded by
// a worker (entry/err set before done is closed); large ones are marked
// stream and archived by the writer itself.
type job struct {
	sf     scannedFile
	stream bool
	entry  *preparedEntry
	err    error
	done   chan struct{}
}

type prepareFunc func(ctx context.Context, sf scannedFile) (*preparedEntry, bool, error)

// pipeline encodes small files on several workers while keeping the
// original order for the single writer. At most inflight jobs are queued
// ahead of the writer, which bounds memory to roughly
// (inflight + workers) * smallFileLimit.
type pipeline struct {
	jobs   chan *job
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func startPipeline(ctx context.Context, files []scannedFile, workers int, prepare prepareFunc) *pipeline {
	ctx, cancel := context.WithCancel(ctx)
	inflight := 2 * workers
	p := &pipeline{jobs: make(chan *job, inflight), cancel: cancel}
	work := make(chan *job)

	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for j := range work {
				if ctx.Err() == nil {
					var tooBig bool
					j.entry, tooBig, j.err = prepare(ctx, j.sf)
					j.stream = tooBig
				}
				close(j.done)
			}
		}()
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.jobs)
		defer close(work)
		for _, sf := range files {
			j := &job{sf: sf, done: make(chan struct{})}
			if sf.Size > smallFileLimit {
				j.stream = true
				close(j.done)
			}
			select {
			case p.jobs <- j:
			case <-ctx.Done():
				return
			}
			if j.stream {
				continue
			}
			select {
			case work <- j:
			case <-ctx.Done():
				close(j.done)
				return
			}
		}
	}()
	return p
}

// next returns the next job once it is ready, or nil when all are done or
// the pipeline was stopped.
func (p *pipeline) next() *job {
	j, ok := <-p.jobs
	if !ok {
		return nil
	}
	<-j.done
	return j
}

// stop cancels outstanding work and waits for all goroutines to exit.
func (p *pipeline) stop() {
	p.cancel()
	for range p.jobs {
	}
	p.wg.Wait()
}

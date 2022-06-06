package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/odpf/entropy/pkg/errors"
)

// Worker provides asynchronous job processing using a job-queue.
type Worker struct {
	workers int
	pollInt time.Duration

	queue    JobQueue
	logger   *zap.Logger
	handlers map[string]JobFn
}

// JobFn is invoked by the Worker for ready jobs. If it returns no error,
// job will be marked with StatusDone. If it returns RetryableError, the
// job will remain in StatusPending and will be enqueued for retry. If
// it returns any other error, job will be marked as StatusFailed. In case
// if a panic occurs, job will be marked as StatusPanic.
type JobFn func(ctx context.Context, job Job) ([]byte, error)

type Option func(w *Worker) error

func New(queue JobQueue, opts ...Option) (*Worker, error) {
	w := &Worker{queue: queue}
	for _, opt := range withDefaults(opts) {
		if err := opt(w); err != nil {
			return nil, err
		}
	}

	if len(w.handlers) == 0 {
		return nil, errors.New("at-least one job handler must be registered")
	}
	return w, nil
}

// Enqueue enqueues all jobs for processing.
func (w *Worker) Enqueue(ctx context.Context, jobs ...Job) error {
	for i, job := range jobs {
		if err := job.Sanitise(); err != nil {
			return err
		} else if _, knownKind := w.handlers[job.Kind]; !knownKind {
			return fmt.Errorf("%w: kind '%s'", ErrUnknownKind, job.Kind)
		}
		jobs[i] = job
	}

	return w.queue.Enqueue(ctx, jobs...)
}

// Run starts the worker threads that dequeue and process ready jobs. Run blocks
// until all workers exit or context is cancelled. Context cancellation will do
// graceful shutdown of the worker threads.
func (w *Worker) Run(baseCtx context.Context) error {
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	wg := &sync.WaitGroup{}
	for i := 0; i < w.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.runWorker(ctx)
			w.logger.Info("worker exited", zap.Int("worker_id", id))
		}(i)
	}
	wg.Wait()

	w.logger.Info("all workers-threads exited")
	return cleanupCtxErr(ctx.Err())
}

func (w *Worker) runWorker(ctx context.Context) {
	timer := time.NewTimer(w.pollInt)
	defer timer.Stop()

	var kinds []string
	for kind := range w.handlers {
		kinds = append(kinds, kind)
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			timer.Reset(w.pollInt)

			w.logger.Info("looking for a job")
			if err := w.queue.Dequeue(ctx, kinds, w.handleJob); err != nil {
				w.logger.Error("dequeue failed", zap.Error(err))
			}
		}
	}
}

func (w *Worker) handleJob(ctx context.Context, job Job) (*Job, error) {
	const invalidKindBackoff = 5 * time.Minute

	fn, exists := w.handlers[job.Kind]
	if !exists {
		// Note: This should never happen since Dequeue() has `kinds` filter.
		//       It is only kept as a safety net to prevent nil-dereferences.
		return nil, &RetryableError{
			Cause:      errors.New("job kind is invalid"),
			RetryAfter: invalidKindBackoff,
		}
	}

	job.Attempt(ctx, time.Now(), fn)
	return &job, nil
}

func cleanupCtxErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

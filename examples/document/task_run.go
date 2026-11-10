package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/sqlite"
)

var ErrTaskOutcomeUnknown = errors.New("document: original execution outcome is unknown; it will not be replayed")

type TaskRunStatus string

const (
	TaskRunActive         TaskRunStatus = "active"
	TaskSettlementPending TaskRunStatus = "settlement_pending"
	TaskOutcomeUnknown    TaskRunStatus = "outcome_unknown"
)

func (TaskRunStatus) Values() []string {
	return []string{string(TaskRunActive), string(TaskSettlementPending), string(TaskOutcomeUnknown)}
}

type taskRunPhase uint8

const (
	taskRunPreparing taskRunPhase = iota
	taskRunExecuting
	taskRunExited
)

type taskConclusion struct {
	state   TaskState
	payload []byte
	failure string
	cause   error
}

func concludeTask(result TaskResult, cause error) *taskConclusion {
	conclusion := &taskConclusion{state: Completed}
	if cause == nil {
		conclusion.payload, cause = json.Marshal(result)
		if len(conclusion.payload) > maximumTaskBytes {
			cause = frame.ErrCapacity
		}
	}
	if cause != nil {
		conclusion.state = Failed
		conclusion.payload = nil
		conclusion.failure = cause.Error()
		conclusion.cause = cause
	}
	return conclusion
}

type taskSettlement struct {
	done chan struct{}
	err  error
}

// Execution exit and durable settlement are separate facts. The original work
// keeps the Store alive while a known result or explicit decision awaits storage.
type taskRun struct {
	app        *App
	id         string
	epoch      uint64
	work       *frame.Work
	mu         sync.Mutex
	phase      taskRunPhase
	conclusion *taskConclusion
	pending    *taskSettlement
	settled    bool
}

func (a *App) taskRun(id string) *taskRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[id]
}

func (a *App) claimTaskRun(ctx context.Context, id string) (*taskRun, bool, frame.ScopeGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if original := a.runs[id]; original != nil {
		return original, false, a.scope, nil
	}
	work, err := a.lifetime.Enter(ctx)
	if err != nil {
		return nil, false, frame.ScopeGrant{}, err
	}
	run := &taskRun{app: a, id: id, epoch: a.Store.Epoch(), work: work}
	a.runs[id] = run
	return run, true, a.scope, nil
}

func (r *taskRun) start() {
	r.mu.Lock()
	r.phase = taskRunExecuting
	r.mu.Unlock()
}

func (r *taskRun) exit() {
	r.mu.Lock()
	released := r.phase == taskRunPreparing || r.settled
	r.phase = taskRunExited
	r.mu.Unlock()
	if released {
		r.release()
	}
}

func (r *taskRun) release() {
	r.app.mu.Lock()
	if r.app.runs[r.id] == r {
		delete(r.app.runs, r.id)
	}
	r.app.mu.Unlock()
	r.work.Finish()
}

func (r *taskRun) project(task *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	task.RunStatus = TaskRunActive
	if r.phase != taskRunExited {
		return nil
	}
	task.RunStatus = TaskOutcomeUnknown
	if r.conclusion == nil {
		return nil
	}
	task.RunStatus = TaskSettlementPending
	task.Failure = r.conclusion.failure
	if len(r.conclusion.payload) != 0 {
		return json.Unmarshal(r.conclusion.payload, &task.Result)
	}
	return nil
}

func (r *taskRun) complete(ctx context.Context, result TaskResult, cause error) error {
	conclusion := concludeTask(result, cause)
	r.mu.Lock()
	r.conclusion = conclusion
	r.mu.Unlock()
	return errors.Join(conclusion.cause, r.settle(ctx))
}

func (r *taskRun) resume(ctx context.Context) (Task, error) {
	r.mu.Lock()
	phase, conclusion := r.phase, r.conclusion
	r.mu.Unlock()
	if phase != taskRunExited {
		return Task{}, frame.ErrInUse
	}
	if conclusion == nil {
		task, err := r.app.Task(ctx, r.id)
		return task, errors.Join(ErrTaskOutcomeUnknown, err)
	}
	task, err := r.app.Task(ctx, r.id)
	if err != nil {
		return task, errors.Join(conclusion.cause, err)
	}
	if err := r.settle(ctx); err != nil {
		return task, errors.Join(conclusion.cause, err)
	}
	task.State = conclusion.state
	task.RunStatus = ""
	task.Failure = conclusion.failure
	if len(conclusion.payload) != 0 {
		err = json.Unmarshal(conclusion.payload, &task.Result)
	}
	return task, errors.Join(conclusion.cause, err)
}

func (r *taskRun) cancel(ctx context.Context) error {
	if ctx == nil {
		return frame.ErrInvalid
	}
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if r.phase != taskRunExited {
		r.mu.Unlock()
		return frame.ErrInUse
	}
	if r.conclusion == nil {
		r.conclusion = &taskConclusion{state: Cancelled}
	}
	r.mu.Unlock()
	return r.settle(ctx)
}

func (a *App) settlePendingTasks(ctx context.Context) error {
	a.mu.Lock()
	runs := make([]*taskRun, 0, len(a.runs))
	for _, run := range a.runs {
		runs = append(runs, run)
	}
	a.mu.Unlock()
	var failures []error
	for _, run := range runs {
		run.mu.Lock()
		pending := run.phase == taskRunExited && run.conclusion != nil
		run.mu.Unlock()
		if pending {
			failures = append(failures, run.settle(ctx))
		}
	}
	return errors.Join(failures...)
}

func (r *taskRun) settle(ctx context.Context) (err error) {
	if ctx == nil {
		return frame.ErrInvalid
	}
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		return nil
	}
	if pending := r.pending; pending != nil {
		r.mu.Unlock()
		select {
		case <-pending.done:
			return pending.err
		case <-ctx.Done():
			return cancellation.Err(ctx)
		}
	}
	pending := &taskSettlement{done: make(chan struct{})}
	r.pending = pending
	conclusion := r.conclusion
	r.mu.Unlock()
	err = errors.New("document: task settlement exited without returning")
	defer func() {
		r.mu.Lock()
		pending.err = err
		r.pending = nil
		r.settled = err == nil
		released := r.settled && r.phase == taskRunExited
		close(pending.done)
		r.mu.Unlock()
		if released {
			r.release()
		}
	}()
	err = r.persist(ctx, conclusion)
	return err
}

func (r *taskRun) persist(ctx context.Context, conclusion *taskConclusion) error {
	var state TaskState
	var epoch uint64
	var payload []byte
	var failure string
	err := r.app.Store.Read(ctx, func(tx sqlite.Reader) error {
		return tx.QueryRowContext(ctx, `SELECT state,run_epoch,result,failure FROM document_tasks WHERE id=?`, r.id).Scan(&state, &epoch, &payload, &failure)
	})
	if err != nil {
		return err
	}
	if state.terminal() {
		if epoch != r.epoch || state != conclusion.state || !bytes.Equal(payload, conclusion.payload) || failure != conclusion.failure {
			return sqlite.ErrConflict
		}
		return nil
	}
	return r.app.owner.Update(ctx, func(ctx context.Context, tx sqlite.Executor) error {
		return changedTask(tx.ExecContext(ctx, `UPDATE document_tasks SET state=?,result=?,failure=? WHERE id=? AND state=? AND run_epoch=?`, conclusion.state, conclusion.payload, conclusion.failure, r.id, Unsettled, r.epoch))
	})
}

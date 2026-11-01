package document

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/sqlite"
)

const taskPolicy = "documents.convert-and-validate/1"

type TaskState string

const (
	Queued    TaskState = "queued"
	Running   TaskState = "running"
	Completed TaskState = "completed"
	Failed    TaskState = "failed"
	Cancelled TaskState = "cancelled"
)

func (TaskState) Values() []string {
	return []string{string(Queued), string(Running), string(Completed), string(Failed), string(Cancelled)}
}

type TaskDefinition struct {
	ID      string                `json:"id"`
	Source  DocumentRef           `json:"source"`
	Policy  string                `json:"policy"`
	Binding frame.BindingSnapshot `json:"binding"`
}
type TaskResult struct {
	Converted   Converted    `json:"converted"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}
type Task struct {
	Submission sqlite.SubmissionID `json:"submission"`
	Definition TaskDefinition      `json:"definition"`
	State      TaskState           `json:"state"`
	RunEpoch   uint64              `json:"runEpoch"`
	Result     *TaskResult         `json:"result,omitempty"`
	Failure    string              `json:"failure,omitempty"`
}

// PreparedTask retains the original request and capture until CommitTask can
// validate it at the same admission boundary as the business-table mutation.
type PreparedTask struct {
	Submission sqlite.SubmissionID
	definition TaskDefinition
	request    []byte
	plan       *frame.BindingPlan
}

func (p *PreparedTask) Definition() TaskDefinition {
	if p == nil {
		return TaskDefinition{}
	}
	result := p.definition
	result.Binding = result.Binding.Clone()
	return result
}

func taskSchema() sqlite.OwnerSchema {
	return sqlite.OwnerSchema{ID: "document.tasks", Statements: []string{`CREATE TABLE document_tasks (id TEXT PRIMARY KEY,admission_sequence INTEGER NOT NULL UNIQUE,definition BLOB NOT NULL,state TEXT NOT NULL CHECK(state IN ('queued','running','completed','failed','cancelled')),run_epoch INTEGER NOT NULL DEFAULT 0,result BLOB,failure TEXT NOT NULL DEFAULT '')`}, References: func(ctx context.Context, tx sqlite.Executor, id frame.InstallationID) ([]frame.Dependency, error) {
		rows, err := tx.QueryContext(ctx, `SELECT definition FROM document_tasks WHERE state IN ('queued','running')`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var dependencies []frame.Dependency
		for rows.Next() {
			var encoded []byte
			if err := rows.Scan(&encoded); err != nil {
				return nil, err
			}
			var definition TaskDefinition
			if err := json.Unmarshal(encoded, &definition); err != nil {
				return nil, err
			}
			if err := definition.Binding.Validate(); err != nil {
				return nil, err
			}
			for _, dependency := range definition.Binding.Dependencies() {
				if dependency.InstallationID == id {
					dependencies = append(dependencies, dependency)
				}
			}
		}
		return frame.CompactDependencies(dependencies), rows.Err()
	}}
}

func (a *App) Reference(ctx context.Context, path string) (DocumentRef, error) {
	if !fs.ValidPath(path) || path == "." {
		return DocumentRef{}, errors.New("document: a relative document path is required")
	}
	records, err := a.Store.Installations(ctx)
	if err != nil {
		return DocumentRef{}, err
	}
	var configuration FileConfiguration
	for _, record := range records {
		if record.ID == "files" {
			if err := json.Unmarshal(record.Configuration, &configuration); err != nil {
				return DocumentRef{}, err
			}
		}
	}
	if configuration.Directory == "" {
		return DocumentRef{}, frame.ErrUnavailable
	}
	root, err := os.OpenRoot(configuration.Directory)
	if err != nil {
		return DocumentRef{}, err
	}
	defer root.Close()
	file, err := root.Open(path)
	if err != nil {
		return DocumentRef{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return DocumentRef{}, err
	}
	if !info.Mode().IsRegular() {
		return DocumentRef{}, errors.New("document: input must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return DocumentRef{}, err
	}
	if len(data) > 4<<20 {
		return DocumentRef{}, frame.ErrCapacity
	}
	if err := ctx.Err(); err != nil {
		return DocumentRef{}, err
	}
	return DocumentRef{Path: path, Revision: integrity.Hash(data)}, nil
}

func (a *App) PrepareTask(ctx context.Context, id string, source DocumentRef) (*PreparedTask, error) {
	if frame.InstallationID(id).Validate() != nil || !fs.ValidPath(source.Path) || source.Revision.Validate() != nil {
		return nil, frame.ErrInvalid
	}
	plan, err := a.plan(ctx)
	if err != nil {
		return nil, err
	}
	binding, err := plan.Snapshot()
	if err != nil {
		return nil, err
	}
	definition := TaskDefinition{ID: id, Source: source, Policy: taskPolicy, Binding: binding}
	request, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}
	submission, err := a.owner.Prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	return &PreparedTask{Submission: submission, definition: definition, request: request, plan: plan}, nil
}

func (a *App) CommitTask(ctx context.Context, prepared *PreparedTask) (sqlite.Settlement, error) {
	if prepared == nil {
		return sqlite.Settlement{}, frame.ErrInvalid
	}
	return a.owner.Commit(ctx, prepared.Submission, prepared.request, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
		binding, err := a.Host.ValidatePlanUnder(ctx, prepared.plan)
		if err != nil {
			return nil, err
		}
		left, err := binding.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		right, err := prepared.definition.Binding.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(left, right) {
			return nil, frame.ErrStaleBinding
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_tasks(id,admission_sequence,definition,state) VALUES(?,?,?,'queued')`, prepared.definition.ID, prepared.Submission.Sequence, prepared.request); err != nil {
			return nil, err
		}
		return []byte(prepared.definition.ID), nil
	}, nil)
}

func (a *App) ObserveSubmission(ctx context.Context, prepared *PreparedTask) (sqlite.Settlement, error) {
	if prepared == nil {
		return sqlite.Settlement{}, frame.ErrInvalid
	}
	return a.owner.Settlement(ctx, prepared.Submission, prepared.request)
}

func (a *App) Queue(ctx context.Context, id, path string) (Task, sqlite.SubmissionID, error) {
	if original, err := a.Task(ctx, id); err == nil {
		if original.Definition.Source.Path != path {
			return Task{}, sqlite.SubmissionID{}, sqlite.ErrConflict
		}
		return original, original.Submission, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sqlite.SubmissionID{}, err
	}
	reference, err := a.Reference(ctx, path)
	if err != nil {
		return Task{}, sqlite.SubmissionID{}, err
	}
	prepared, err := a.PrepareTask(ctx, id, reference)
	if err != nil {
		return Task{}, sqlite.SubmissionID{}, err
	}
	settlement, err := a.CommitTask(ctx, prepared)
	if err != nil || settlement.State != sqlite.Committed {
		if original, observed := a.Task(ctx, id); observed == nil && original.Definition.Source.Path == path {
			return original, original.Submission, nil
		}
		return Task{}, prepared.Submission, errors.Join(err, fmt.Errorf("document: admission is %s", settlement.State))
	}
	task, err := a.Task(ctx, id)
	return task, prepared.Submission, err
}

func (a *App) Task(ctx context.Context, id string) (Task, error) {
	var task Task
	task.Submission = sqlite.SubmissionID{Store: a.Store.ID(), Owner: "document.tasks"}
	err := a.Store.Read(ctx, func(tx sqlite.Executor) error {
		var definition, result []byte
		if err := tx.QueryRowContext(ctx, `SELECT admission_sequence,definition,state,run_epoch,result,failure FROM document_tasks WHERE id=?`, id).Scan(&task.Submission.Sequence, &definition, &task.State, &task.RunEpoch, &result, &task.Failure); err != nil {
			return err
		}
		if err := json.Unmarshal(definition, &task.Definition); err != nil {
			return err
		}
		if err := task.Definition.Binding.Validate(); err != nil {
			return err
		}
		if len(result) != 0 {
			return json.Unmarshal(result, &task.Result)
		}
		return nil
	})
	return task, err
}

func (a *App) readQueuedBinding(ctx context.Context, id string, grant frame.ScopeGrant) (frame.BindingSnapshot, error) {
	a.mu.Lock()
	authorized := a.scope == grant
	a.mu.Unlock()
	if !authorized {
		return frame.BindingSnapshot{}, frame.ErrFenced
	}
	task, err := a.Task(ctx, id)
	if err != nil {
		return frame.BindingSnapshot{}, err
	}
	if task.State != Queued || task.Definition.ID != id || task.Definition.Policy != taskPolicy {
		return frame.BindingSnapshot{}, frame.ErrUnavailable
	}
	return task.Definition.Binding, nil
}

func (a *App) Run(ctx context.Context, id string) (returned Task, returnErr error) {
	a.mu.Lock()
	if a.running[id] {
		a.mu.Unlock()
		return Task{}, frame.ErrInUse
	}
	a.running[id] = true
	scope := a.scope
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.running, id); a.mu.Unlock() }()
	task, err := a.Task(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.State == Completed || task.State == Failed || task.State == Cancelled {
		return task, nil
	}
	if task.State != Queued {
		return task, errors.New("document: original execution outcome is unknown; it will not be replayed")
	}
	if task.Definition.Policy != taskPolicy {
		return task, errors.New("document: retained policy is unavailable")
	}
	use, err := a.Host.AcquireRecord(ctx, id, frame.UseConfig{Owner: a.lifetime, Scope: scope})
	if err != nil {
		return task, err
	}
	defer func() {
		outcome, err := use.Close(context.WithoutCancel(ctx))
		returnErr = errors.Join(returnErr, err)
		if !outcome.Released {
			returnErr = errors.Join(returnErr, frame.ErrUnconfirmedRelease)
		}
	}()
	startRequest, _ := json.Marshal(struct {
		Operation string
		ID        string
		Epoch     uint64
	}{"start", id, a.Store.Epoch()})
	start, err := a.owner.Prepare(ctx, startRequest)
	if err != nil {
		return task, err
	}
	settlement, err := a.owner.Commit(ctx, start, startRequest, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
		if err := a.Host.ValidateBindingUnder(ctx, task.Definition.Binding); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE document_tasks SET state='running',run_epoch=? WHERE id=? AND state='queued'`, a.Store.Epoch(), id)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			return nil, ErrTaskFinished
		}
		return []byte(id), nil
	}, nil)
	if err != nil || settlement.State != sqlite.Committed {
		return task, errors.Join(err, sqlite.ErrUnsettled)
	}
	result := TaskResult{Diagnostics: []Diagnostic{}}
	var runErr error
	converted := false
	for _, source := range task.Definition.Binding.Roots {
		switch source.Contract {
		case a.Contracts.Converters.Ref():
			if converted {
				runErr = frame.ErrAmbiguous
				break
			}
			converted = true
			runErr = frame.BorrowExport(ctx, use, source, func(ctx context.Context, converter Converter) error {
				var err error
				result.Converted, err = converter(ctx, task.Definition.Source)
				return err
			})
		case a.Contracts.Validators.Ref():
			if !converted {
				runErr = frame.ErrInvalid
				break
			}
			runErr = frame.BorrowExport(ctx, use, source, func(ctx context.Context, validator Validator) error {
				diagnostics, err := validator(ctx, result.Converted)
				result.Diagnostics = append(result.Diagnostics, diagnostics...)
				return err
			})
		default:
			runErr = frame.ErrContractMismatch
		}
		if runErr != nil {
			break
		}
	}
	if !converted && runErr == nil {
		runErr = frame.ErrUnavailable
	}
	if len(result.Converted.HTML) > 8<<20 {
		runErr = frame.ErrCapacity
	}
	state := Completed
	failure := ""
	var storedResult *TaskResult = &result
	if runErr != nil {
		state = Failed
		failure = runErr.Error()
		storedResult = nil
	}
	finishRequest, err := json.Marshal(struct {
		Operation string
		ID        string
		Epoch     uint64
		State     TaskState
		Result    *TaskResult
		Failure   string
	}{"finish", id, a.Store.Epoch(), state, storedResult, failure})
	if err != nil {
		return task, err
	}
	settlementContext := context.WithoutCancel(ctx)
	finish, err := a.owner.Prepare(settlementContext, finishRequest)
	if err != nil {
		return task, errors.Join(runErr, err)
	}
	_, err = a.owner.Commit(settlementContext, finish, finishRequest, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
		var encoded []byte
		if storedResult != nil {
			var err error
			encoded, err = json.Marshal(storedResult)
			if err != nil {
				return nil, err
			}
		}
		changed, err := tx.ExecContext(ctx, `UPDATE document_tasks SET state=?,result=?,failure=? WHERE id=? AND state='running' AND run_epoch=?`, state, encoded, failure, id, a.Store.Epoch())
		if err != nil {
			return nil, err
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, ErrTaskFinished
		}
		return []byte(id), nil
	}, nil)
	if err != nil {
		return task, errors.Join(runErr, err)
	}
	finished, err := a.Task(settlementContext, id)
	return finished, errors.Join(runErr, err)
}

// Cancel only changes an idle queued task or an original execution whose Host
// epoch has ended. A current native callback must first observe cancellation
// and join through its Run owner; this method cannot declare it stopped.
func (a *App) Cancel(ctx context.Context, id string) error {
	request, _ := json.Marshal(struct{ Operation, ID string }{"cancel", id})
	submission, err := a.owner.Prepare(ctx, request)
	if err != nil {
		return err
	}
	_, err = a.owner.Commit(ctx, submission, request, func(ctx context.Context, tx sqlite.Executor) ([]byte, error) {
		var state TaskState
		var epoch uint64
		if err := tx.QueryRowContext(ctx, `SELECT state,run_epoch FROM document_tasks WHERE id=?`, id).Scan(&state, &epoch); err != nil {
			return nil, err
		}
		if state == Completed || state == Failed || state == Cancelled {
			return []byte(id), nil
		}
		if state == Running && epoch == a.Store.Epoch() {
			return nil, frame.ErrInUse
		}
		_, err := tx.ExecContext(ctx, `UPDATE document_tasks SET state='cancelled' WHERE id=?`, id)
		return []byte(id), err
	}, nil)
	return err
}

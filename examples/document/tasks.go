package document

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

const maximumTaskBytes = 32 << 20
const maximumConvertedHTMLBytes = 8 << 20
const taskOwnerID = "document.tasks"
const taskPolicy = "documents.convert-and-validate/1"

type TaskState string

const (
	Queued    TaskState = "queued"
	Running   TaskState = "running"
	Completed TaskState = "completed"
	Failed    TaskState = "failed"
	Cancelled TaskState = "cancelled"
)

func (state TaskState) terminal() bool {
	return state == Completed || state == Failed || state == Cancelled
}

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
	states := TaskState("").Values()
	for i, state := range states {
		states[i] = "'" + strings.ReplaceAll(state, "'", "''") + "'"
	}
	taskTable := fmt.Sprintf(`CREATE TABLE document_tasks (id TEXT PRIMARY KEY,admission_sequence INTEGER NOT NULL UNIQUE,definition BLOB NOT NULL,state TEXT NOT NULL CHECK(state IN (%s)),run_epoch INTEGER NOT NULL DEFAULT 0,result BLOB,failure TEXT NOT NULL DEFAULT '')`, strings.Join(states, ","))
	return sqlite.OwnerSchema{ID: taskOwnerID, Statements: []string{taskTable, `CREATE TABLE document_bootstrap (singleton INTEGER PRIMARY KEY CHECK(singleton=1))`}, References: func(ctx context.Context, tx sqlite.Reader, id frame.InstallationID) ([]frame.Dependency, error) {
		rows, err := tx.QueryContext(ctx, `SELECT definition FROM document_tasks WHERE state IN (?,?)`, Queued, Running)
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
	records, err := a.Store.Installations(ctx)
	if err != nil {
		return DocumentRef{}, err
	}
	var configuration FileConfiguration
	for _, record := range records {
		if record.ID == fileInstallationID {
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
	source, err := loadDocument(ctx, root, path)
	return source.Ref, err
}

func (a *App) PrepareTask(ctx context.Context, id string, source DocumentRef) (*PreparedTask, error) {
	if frame.InstallationID(id).Validate() != nil || !fs.ValidPath(source.Path) || source.Path == "." || source.Revision.Validate() != nil {
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
	var submission sqlite.SubmissionID
	err = a.Host.Admission().Admit(ctx, func(admitted context.Context) error {
		var err error
		submission, err = a.owner.PrepareUnder(admitted, request)
		if !errors.Is(err, frame.ErrCapacity) {
			return err
		}
		// Task rows keep their original identity after synchronized receipts expire.
		if err := a.owner.CompactSettledUnder(admitted); err != nil {
			return err
		}
		submission, err = a.owner.PrepareUnder(admitted, request)
		return err
	})
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_tasks(id,admission_sequence,definition,state) VALUES(?,?,?,?)`, prepared.definition.ID, prepared.Submission.Sequence, prepared.request, Queued); err != nil {
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
	task.Submission = sqlite.SubmissionID{Store: a.Store.ID(), Owner: taskOwnerID}
	err := a.Store.Read(ctx, func(tx sqlite.Reader) error {
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
	if task.State.terminal() {
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
	if err := a.startTask(ctx, task); err != nil {
		return task, err
	}
	result, runErr := a.convert(ctx, task, use)
	var payload []byte
	if runErr == nil {
		payload, runErr = json.Marshal(result)
		if len(payload) > maximumTaskBytes {
			payload = nil
			runErr = frame.ErrCapacity
		}
	}
	finishCtx := context.WithoutCancel(ctx)
	if err := a.finishTask(finishCtx, id, payload, runErr); err != nil {
		return task, errors.Join(runErr, err)
	}
	finished, err := a.Task(finishCtx, id)
	return finished, errors.Join(runErr, err)
}

func changedTask(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrTaskFinished
	}
	return nil
}

func (a *App) startTask(ctx context.Context, task Task) error {
	return a.owner.Update(ctx, func(ctx context.Context, tx sqlite.Executor) error {
		if err := a.Host.ValidateBindingUnder(ctx, task.Definition.Binding); err != nil {
			return err
		}
		return changedTask(tx.ExecContext(ctx, `UPDATE document_tasks SET state=?,run_epoch=? WHERE id=? AND state=?`, Running, a.Store.Epoch(), task.Definition.ID, Queued))
	})
}

func (a *App) convert(ctx context.Context, task Task, use *frame.Use) (TaskResult, error) {
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
	if len(result.Converted.HTML) > maximumConvertedHTMLBytes {
		runErr = frame.ErrCapacity
	}
	return result, runErr
}

func (a *App) finishTask(ctx context.Context, id string, payload []byte, runErr error) error {
	state := Completed
	var failure string
	if runErr != nil {
		state, failure = Failed, runErr.Error()
	}
	return a.owner.Update(ctx, func(ctx context.Context, tx sqlite.Executor) error {
		return changedTask(tx.ExecContext(ctx, `UPDATE document_tasks SET state=?,result=?,failure=? WHERE id=? AND state=? AND run_epoch=?`, state, payload, failure, id, Running, a.Store.Epoch()))
	})
}

// Cancel only changes an idle queued task or an original execution whose Host
// epoch has ended. A current native callback must first observe cancellation
// and join through its Run owner; this method cannot declare it stopped.
func (a *App) Cancel(ctx context.Context, id string) error {
	return a.owner.Update(ctx, func(ctx context.Context, tx sqlite.Executor) error {
		var state TaskState
		var epoch uint64
		if err := tx.QueryRowContext(ctx, `SELECT state,run_epoch FROM document_tasks WHERE id=?`, id).Scan(&state, &epoch); err != nil {
			return err
		}
		if state.terminal() {
			return nil
		}
		if state == Running && epoch == a.Store.Epoch() {
			return frame.ErrInUse
		}
		return changedTask(tx.ExecContext(ctx, `UPDATE document_tasks SET state=? WHERE id=?`, Cancelled, id))
	})
}

package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

type submissionScanner interface{ Scan(...any) error }

type submissionRecord struct {
	sequence uint64
	digest   value.Digest
	request  []byte
	state    submissionState
	evidence []byte
}

func (r submissionRecord) settlement(id SubmissionID) (Settlement, error) {
	state, err := r.state.settlement()
	if r.sequence == 0 || len(r.request) == 0 || r.digest != value.Hash(r.request) || state != frame.CommitSucceeded && r.evidence != nil {
		err = errors.Join(err, ErrIndeterminate, errors.New("sqlite: invalid stored submission evidence"))
	}
	if err != nil {
		return Settlement{}, err
	}
	id.Sequence = r.sequence
	return Settlement{ID: id, State: state, Digest: r.digest, Evidence: r.evidence, Synchronized: r.state.synchronized()}, nil
}

func (o Owner) scanSubmission(row submissionScanner) (Settlement, []byte, submissionState, error) {
	var record submissionRecord
	if err := row.Scan(&record.sequence, &record.digest, &record.request, &record.state, &record.evidence); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, value.ErrInvalidDigest) {
			err = o.corruptSubmission(err)
		}
		return Settlement{}, nil, record.state, err
	}
	result, err := record.settlement(SubmissionID{Store: o.store.id, Owner: o.id})
	if err != nil {
		err = o.corruptSubmission(err)
	}
	return result, record.request, record.state, err
}

type submissionRange struct {
	issued    uint64
	compacted uint64
}

func (r submissionRange) require(sequence uint64) error {
	if sequence <= r.compacted {
		return ErrEvidenceExpired
	}
	if sequence > r.issued {
		return ErrUnknownSubmission
	}
	return nil
}

func (r submissionRange) verify(count, first, last uint64) error {
	// Unique ordered sequences fill the interval only when both endpoints and
	// its cardinality agree with the original issuance and compaction watermarks.
	if count != r.issued-r.compacted || count != 0 && (first != r.compacted+1 || last != r.issued) {
		return errors.New("sqlite: incomplete retained submission evidence")
	}
	return nil
}

func (o Owner) readSubmissionRange(ctx context.Context, tx Reader) (submissionRange, error) {
	var issued, compacted int64
	if err := tx.QueryRowContext(ctx, `SELECT next_sequence,compacted_through FROM frame_owners WHERE id=?`, o.id).Scan(&issued, &compacted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = o.corruptSubmission(err)
		}
		return submissionRange{}, err
	}
	if compacted < 0 || issued < compacted {
		return submissionRange{}, o.corruptSubmission(errors.New("sqlite: invalid submission watermarks"))
	}
	return submissionRange{issued: uint64(issued), compacted: uint64(compacted)}, nil
}

func (o Owner) readSubmission(ctx context.Context, tx Reader, id SubmissionID) (Settlement, []byte, submissionState, error) {
	retained, err := o.readSubmissionRange(ctx, tx)
	if err != nil {
		return Settlement{}, nil, "", err
	}
	if err := retained.require(id.Sequence); err != nil {
		return Settlement{}, nil, "", err
	}
	return o.scanSubmission(tx.QueryRowContext(ctx, `SELECT sequence,digest,request,state,evidence FROM frame_submissions WHERE owner_id=? AND sequence=?`, o.id, id.Sequence))
}

func (o Owner) retainedSubmissions(ctx context.Context, tx Reader) (uint64, error) {
	retained, err := o.readSubmissionRange(ctx, tx)
	if err != nil {
		return 0, err
	}
	var count, first, last uint64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0) FROM frame_submissions WHERE owner_id=?`, o.id).Scan(&count, &first, &last); err != nil {
		return 0, err
	}
	if err := retained.verify(count, first, last); err != nil {
		return 0, o.corruptSubmission(err)
	}
	return count, nil
}

func (o Owner) readSubmissions(ctx context.Context, tx Reader) ([]Settlement, error) {
	retained, err := o.readSubmissionRange(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,digest,request,state,evidence FROM frame_submissions WHERE owner_id=? ORDER BY sequence`, o.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Settlement
	for rows.Next() {
		record, _, _, err := o.scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var first, last uint64
	if len(records) != 0 {
		first, last = records[0].ID.Sequence, records[len(records)-1].ID.Sequence
	}
	if err := retained.verify(uint64(len(records)), first, last); err != nil {
		return nil, o.corruptSubmission(err)
	}
	return records, nil
}

func (o Owner) corruptSubmission(err error) error {
	if !errors.Is(err, ErrIndeterminate) {
		err = errors.Join(ErrIndeterminate, err)
	}
	o.store.admission.RequireRecovery(err)
	return err
}

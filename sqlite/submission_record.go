package sqlite

import (
	"context"
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
		if errors.Is(err, value.ErrInvalidDigest) {
			err = errors.Join(ErrIndeterminate, err)
			o.store.admission.RequireRecovery(err)
		}
		return Settlement{}, nil, record.state, err
	}
	result, err := record.settlement(SubmissionID{Store: o.store.id, Owner: o.id})
	if err != nil {
		o.store.admission.RequireRecovery(err)
	}
	return result, record.request, record.state, err
}

func (o Owner) readSubmissions(ctx context.Context, tx Reader) ([]Settlement, error) {
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
	return records, rows.Err()
}

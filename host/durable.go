package host

// Authority supplies the admission gate a Host enters. An Admission alone is a
// volatile authority: nothing it admits outlives the process.
type Authority interface {
	Admission() Admission
}

// Durable is an Authority that also persists installations and protects
// surviving executions. It owns the creation epoch fencing its records, so the
// Host derives the gate and the epoch from it instead of holding copies.
type Durable interface {
	Authority
	Epoch() uint64
	InstallationStore
	ExecutionProtection
}

func (a Admission) Admission() Admission { return a }

// volatileEpoch numbers the single creation generation of a Host without a
// Durable authority: no older epoch can hold work that must be fenced.
const volatileEpoch = 1

func (h *hostState) Epoch() uint64 {
	if h.durable == nil {
		return volatileEpoch
	}
	return h.durable.Epoch()
}

// Durable lets an adapter that writes to the same store refuse a Setup issued
// by a Host composed with a different one.
func (h *hostState) Durable() Durable { return h.durable }

func (s Setup) Durable() Durable {
	if s.setupCall == nil {
		return nil
	}
	return s.host.durable
}

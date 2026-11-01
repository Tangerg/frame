package process

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

// Protocol binds a Frame execution profile to a conversation on an attached
// child. Prepare must validate the frozen declaration before launch and must
// not acquire resources. The driver alone owns launch, attach and exit proof.
type Protocol interface {
	Profile() frame.Profile
	Prepare(*frame.Setup, ExportMetadata, int) (Connection, error)
}

// Connection is private to one execution. Close follows every Connect, including
// failed handshakes. The driver interrupts blocked setup and closes the stream.
type Connection interface {
	Connect(context.Context, io.ReadWriteCloser) ([]frame.ExportValue, error)
	Close(context.Context) error
}

// DeclaredCapability resolves the exact portable service through its approved declaration.
func DeclaredCapability(setup *frame.Setup, ref frame.ContractRef) (*portable.Capability, error) {
	if setup == nil {
		return nil, frame.ErrInvalid
	}
	service, err := setup.ValueContract(ref)
	if err != nil {
		return nil, err
	}
	if service.Mode != frame.PortableMode || service.Kind != frame.ServiceKind {
		return nil, frame.ErrContractMismatch
	}
	raw, err := setup.ContractDescriptor(service)
	if err != nil {
		return nil, err
	}
	contract, err := portable.ParseCapability(raw)
	if err != nil {
		return nil, errors.Join(frame.ErrContractMismatch, err)
	}
	if contract.Descriptor().Reference != service.Name || contract.Identity().Digest() != service.Digest {
		return nil, frame.ErrContractMismatch
	}
	return contract, nil
}

// ExportMetadata projects per-export metadata from the approved distribution.
type ExportMetadata map[string]value.JSON

// Export projects one approved declaration and its distribution metadata.
func (m ExportMetadata) Export(decl frame.ExportDeclaration, service Service) frame.ExportValue {
	v := frame.ExportValue{Name: decl.Name, Contract: decl.Contract, Value: service, Durable: true}
	if metadata, exists := m[decl.Name]; exists {
		v.Metadata = metadata
	}
	return v
}

type attachment struct {
	io.ReadWriteCloser
	once sync.Once
	err  error
}

func (a *attachment) Close() error {
	a.once.Do(func() { a.err = a.ReadWriteCloser.Close() })
	return a.err
}

package process

import (
	"context"
	"io"
	"sync"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

// Protocol binds a Frame execution profile to a conversation on an attached
// child. Prepare must validate the frozen declaration before launch and must
// not acquire resources. The driver alone owns launch, attach and exit proof.
type Protocol interface {
	Profile() framehost.Profile
	Prepare(*framehost.Setup, ExportMetadata, int) (Connection, error)
}

// Connection is private to one execution. Close follows every Connect, including
// failed handshakes. The driver interrupts blocked setup and closes the stream.
// Close must join its host-side work before returning; cancellation alone cannot
// establish release, and callers can stop waiting without replacing this owner.
type Connection interface {
	Connect(context.Context, io.ReadWriteCloser) ([]framehost.ExportValue, error)
	Close(context.Context) error
}

type ExportMetadata map[string]value.JSON

func (m ExportMetadata) Export(decl framehost.ExportDeclaration, service frame.Service) framehost.ExportValue {
	v := framehost.ExportValue{Name: decl.Name, Contract: decl.Contract, Value: service}
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

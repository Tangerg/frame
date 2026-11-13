package runner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

// Publication and removal share the Server's directory authority. The socket
// cannot unlink by pathname after that authority has passed to another Server.
type controlListener struct {
	owner    *Server
	socket   *net.UnixListener
	file     os.FileInfo
	closed   bool
	closeErr error
	cancel   context.CancelFunc
	served   chan struct{}
}

func (l *controlListener) Accept() (net.Conn, error) { return l.socket.Accept() }
func (l *controlListener) Addr() net.Addr            { return l.socket.Addr() }

func (l *controlListener) Close() error {
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	return l.close()
}

func (l *controlListener) close() error {
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	if l.cancel != nil {
		l.cancel()
	}
	l.closeErr = l.socket.Close()
	path := l.socket.Addr().String()
	name := filepath.Base(path)
	info, err := l.owner.directory.Lstat(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		l.closeErr = errors.Join(l.closeErr, err)
	case os.SameFile(l.file, info):
		l.closeErr = errors.Join(l.closeErr, l.owner.directory.Remove(name))
	}
	return l.closeErr
}

func (l *controlListener) released() bool {
	if !l.closed {
		return false
	}
	if l.served == nil {
		return true
	}
	select {
	case <-l.served:
		return true
	default:
		return false
	}
}

func (s *Server) Listen(socket string) (net.Listener, error) {
	if err := validateSocketPath(socket); err != nil {
		return nil, err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	if directory != s.config.Directory {
		return nil, errors.New("runner: socket must be in the owned state directory")
	}
	socket = filepath.Join(directory, filepath.Base(socket))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lock == nil {
		return nil, frame.ErrClosed
	}
	if s.listener != nil && !s.listener.released() {
		return nil, ErrConflict
	}
	if err := s.verifyDirectory(); err != nil {
		return nil, err
	}
	name := filepath.Base(socket)
	if info, err := s.directory.Lstat(name); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("runner: control path is not a socket")
		}
		if err := s.directory.Remove(name); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := s.verifyDirectory(); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := s.directory.Chmod(name, localfile.PrivateFileMode); err != nil {
		_ = listener.Close()
		_ = s.directory.Remove(name)
		return nil, err
	}
	info, err := s.directory.Lstat(name)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	s.listener = &controlListener{owner: s, socket: listener, file: info}
	return s.listener, nil
}

func (s *Server) admitServe(ctx context.Context, listener net.Listener) (context.Context, *controlListener, error) {
	owned, ok := listener.(*controlListener)
	if framevalue.IsNil(ctx) || !ok || owned == nil || owned.owner != s {
		return nil, nil, frame.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || owned.closed {
		return nil, nil, frame.ErrClosed
	}
	if owned != s.listener || owned.served != nil {
		return nil, nil, ErrConflict
	}
	if err := s.verifyDirectory(); err != nil {
		return nil, nil, err
	}
	info, err := s.directory.Lstat(filepath.Base(owned.socket.Addr().String()))
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(owned.file, info) {
		return nil, nil, ErrConflict
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&nonOwnerSocketPermissions != 0 {
		return nil, nil, errors.New("runner: listener must be a private Unix socket")
	}
	ctx, owned.cancel = context.WithCancel(ctx)
	owned.served = make(chan struct{})
	return ctx, owned, nil
}

const nonOwnerSocketPermissions os.FileMode = 0077

package runner

import (
	"context"
	"errors"
	"os"
)

type processCloseState uint8

const (
	processOpen processCloseState = iota
	processCloseRequested
	processKillIssued
	processWaitUnconfirmed
)

// The Server mutex serializes this original child's close path. A rejected Kill
// does not complete the request; a later close may advance that same OS handle.
func (p *process) requestClose() error {
	switch p.closeState {
	case processWaitUnconfirmed:
		return ErrUnknown
	case processKillIssued:
		return nil
	case processOpen:
		p.closeState = processCloseRequested
		_ = p.stdin.Close()
	}
	if err := p.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	p.closeState = processKillIssued
	return nil
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.fenced = true
		s.mu.Unlock()
		s.closeRequests = make(chan chan error)
		s.closeDone = make(chan struct{})
		go s.closeOriginal()
	})
	result := make(chan error, 1)
	select {
	case <-s.closeDone:
		return s.closeErr
	case s.closeRequests <- result:
	}
	select {
	case err := <-result:
		return err
	case <-s.closeDone:
		return s.closeErr
	}
}

// This outstanding shutdown keeps the original child handles and directory lock
// alive even if its caller drops the Server after an unconfirmed close.
func (s *Server) closeOriginal() {
	for result := range s.closeRequests {
		err := s.advanceClose()
		if s.lock == nil {
			s.closeErr = err
			close(s.closeDone)
			result <- err
			return
		}
		result <- err
	}
}

func (s *Server) advanceClose() error {
	s.mu.Lock()
	refs := make([]AttemptRef, 0, len(s.processes))
	for id := range s.processes {
		refs = append(refs, s.journal.Attempts[id].Observation.Reference())
	}
	s.mu.Unlock()
	var closeErr error
	for _, ref := range refs {
		_, err := s.CloseAttempt(context.Background(), ref)
		closeErr = errors.Join(closeErr, err)
	}
	s.mu.Lock()
	if len(s.processes) != 0 {
		s.mu.Unlock()
		return errors.Join(ErrUnknown, closeErr)
	}
	if len(s.terminalObservations) != 0 {
		if err := s.persist(); err != nil {
			s.mu.Unlock()
			return errors.Join(ErrUnknown, closeErr, err)
		}
	}
	var served <-chan struct{}
	if s.listener != nil {
		closeErr = errors.Join(closeErr, s.listener.close())
		served = s.listener.served
	}
	s.mu.Unlock()
	if served != nil {
		<-served
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	closeErr = errors.Join(closeErr, unlockDirectory(s.lock), s.directory.Close())
	s.lock = nil
	return closeErr
}

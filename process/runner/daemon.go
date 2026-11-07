package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
)

// DaemonConfig identifies a separately packaged runner executable and its
// private state. Starting the authority never launches a plugin attempt.
type DaemonConfig struct {
	Executable   string
	Directory    string
	StartTimeout time.Duration
}

// Daemon joins one original startup. The process remains the authority after
// this client or its Host exits; a later Host connects to its original journal.
type Daemon struct {
	config        DaemonConfig
	client        *Client
	once          sync.Once
	ready         chan struct{}
	err           error
	mu            sync.Mutex
	started       bool
	closing       bool
	command       *exec.Cmd
	exited        chan struct{}
	exitErr       error
	closeOnce     sync.Once
	closeRequests chan chan error
	closeDone     chan struct{}
	closeErr      error
}

const readinessPollInterval = 10 * time.Millisecond

func NewDaemon(config DaemonConfig) (*Daemon, error) {
	if !filepath.IsAbs(config.Executable) || !filepath.IsAbs(config.Directory) || config.StartTimeout <= 0 {
		return nil, errors.New("runner: daemon requires absolute executable and directory and startup limit")
	}
	socket := filepath.Join(config.Directory, "control.sock")
	client, err := NewClient(socket)
	if err != nil {
		return nil, err
	}
	return &Daemon{config: config, client: client, ready: make(chan struct{}), closeRequests: make(chan chan error), closeDone: make(chan struct{})}, nil
}

func (d *Daemon) connect(ctx context.Context) error {
	if ctx == nil {
		return frame.ErrInvalid
	}
	if err := cancellation.Err(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return frame.ErrClosed
	}
	d.once.Do(func() {
		d.started = true
		go func() {
			owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.config.StartTimeout)
			defer cancel()
			d.err = d.start(owner)
			close(d.ready)
		}()
	})
	d.mu.Unlock()
	select {
	case <-ctx.Done():
		return cancellation.Err(ctx)
	case <-d.ready:
		return d.err
	}
}

func (d *Daemon) start(ctx context.Context) error {
	_, err := d.client.Identity(ctx)
	if err == nil {
		return nil
	}
	// Only a missing listener authorizes starting an authority. A malformed
	// response, denied socket or unknown transport failure is not absence.
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	command := exec.Command(d.config.Executable, "--directory", d.config.Directory, "--socket", d.client.socket)
	if err := detachDaemon(command); err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	d.command = command
	d.exited = make(chan struct{})
	go func() { d.exitErr = command.Wait(); close(d.exited) }()
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()
	for {
		_, err := d.client.Identity(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return err
		}
		select {
		case <-ctx.Done():
			return cancellation.Err(ctx)
		case <-d.exited:
			return errors.Join(errors.New("runner: original daemon exited before readiness"), d.exitErr)
		case <-ticker.C:
		}
	}
}

func (d *Daemon) Identity(ctx context.Context) (Identity, error) {
	if err := d.connect(ctx); err != nil {
		return Identity{}, err
	}
	return d.client.Identity(ctx)
}
func (d *Daemon) Fence(ctx context.Context, epoch uint64) error {
	if err := d.connect(ctx); err != nil {
		return err
	}
	return d.client.Fence(ctx, epoch)
}
func (d *Daemon) Launch(ctx context.Context, request LaunchRequest) (LaunchResult, error) {
	if err := d.connect(ctx); err != nil {
		return notDispatched(request), err
	}
	return d.client.Launch(ctx, request)
}
func (d *Daemon) Attach(ctx context.Context, ref AttemptRef) (io.ReadWriteCloser, error) {
	if err := d.connect(ctx); err != nil {
		return nil, err
	}
	return d.client.Attach(ctx, ref)
}
func (d *Daemon) Inspect(ctx context.Context, id string) (Observation, error) {
	if err := d.connect(ctx); err != nil {
		return Observation{}, err
	}
	return d.client.Inspect(ctx, id)
}
func (d *Daemon) CloseAttempt(ctx context.Context, ref AttemptRef) (Observation, error) {
	if err := d.connect(ctx); err != nil {
		return Observation{}, err
	}
	return d.client.CloseAttempt(ctx, ref)
}

func (d *Daemon) AcknowledgeExit(ctx context.Context, receipt ExitReceipt) error {
	if err := d.connect(ctx); err != nil {
		return err
	}
	return d.client.AcknowledgeExit(ctx, receipt)
}

func (d *Daemon) ProveAbsent(ctx context.Context, request AttemptRef) (AbsenceProof, error) {
	if err := d.connect(ctx); err != nil {
		return AbsenceProof{}, err
	}
	return d.client.ProveAbsent(ctx, request)
}

// Close stops only the daemon this object actually created, after joining its
// startup. A connected pre-existing authority is left with its original owner.
// Consumers order this after all Host resources have actual release proof.
// A later call advances the same shutdown if the daemon retained an unconfirmed
// release; cancellation abandons only this caller's wait.
func (d *Daemon) Close(ctx context.Context) error {
	if ctx == nil {
		return frame.ErrInvalid
	}
	d.mu.Lock()
	d.closing = true
	started := d.started
	d.mu.Unlock()
	result := make(chan error, 1)
	first := false
	d.closeOnce.Do(func() {
		first = true
		go func() {
			if started {
				d.closeErr = d.closeOriginal(result)
			}
			close(d.closeDone)
		}()
	})
	if !first {
		select {
		case <-d.closeDone:
			return d.closeErr
		case d.closeRequests <- result:
		case <-ctx.Done():
			return cancellation.Err(ctx)
		}
	}
	select {
	case <-ctx.Done():
		return cancellation.Err(ctx)
	case err := <-result:
		return err
	case <-d.closeDone:
		return d.closeErr
	}
}

func (d *Daemon) closeOriginal(result chan error) error {
	<-d.ready
	if d.command == nil {
		return nil
	}
	stopIssued := false
	for {
		select {
		case <-d.exited:
			// Startup can end before the executable installs a signal handler. Only
			// normalize our delivered stop after the original Wait proves that exit.
			if stopIssued && interruptedExit(d.exitErr) {
				return nil
			}
			return d.exitErr
		default:
		}
		err := d.command.Process.Signal(os.Interrupt)
		if err == nil {
			stopIssued = true
		} else if !errors.Is(err, os.ErrProcessDone) {
			result <- err
		}
		select {
		case <-d.exited:
		case result = <-d.closeRequests:
		}
	}
}

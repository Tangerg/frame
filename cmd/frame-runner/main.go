package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tangerg/frame/process/runner"
)

func main() {
	directory := flag.String("directory", "", "private runner state directory")
	socket := flag.String("socket", "", "private unix control socket")
	flag.Parse()
	if err := run(*directory, *socket); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(directory, socket string) error {
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopping)
	return serve(directory, socket, stopping, os.Stderr)
}

func serve(directory, socket string, stopping <-chan os.Signal, diagnostics io.Writer) error {
	server, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		return err
	}
	listener, err := server.Listen(socket)
	if err != nil {
		return errors.Join(err, shutdown(server, stopping, diagnostics))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()
	select {
	case <-stopping:
		cancel()
		err = <-served
	case err = <-served:
		cancel()
	}
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, shutdown(server, stopping, diagnostics))
}

func shutdown(server *runner.Server, stopping <-chan os.Signal, diagnostics io.Writer) error {
	for {
		err := server.Close()
		if !errors.Is(err, runner.ErrUnknown) {
			return err
		}
		fmt.Fprintf(diagnostics, "runner: shutdown remains unconfirmed; restore the failed resource and signal again: %v\n", err)
		// Exiting here would destroy the only original Wait observation and lock.
		<-stopping
	}
}

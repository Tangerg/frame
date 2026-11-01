package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	server, err := runner.Open(runner.Config{Directory: directory})
	if err != nil {
		return err
	}
	defer server.Close()
	listener, err := server.Listen(socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = server.Serve(ctx, listener)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tangerg/frame/examples/document"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (returned error) {
	command := flag.String("command", "convert", "convert, queue, run, status or serve")
	directory := flag.String("documents", ".", "document directory")
	state := flag.String("state", ".frame-document", "application state directory")
	path := flag.String("document", "", "relative document path")
	id := flag.String("task", "", "original task identity; generated when omitted")
	listen := flag.String("listen", "127.0.0.1:8088", "HTTP listen address")
	web := flag.String("web", "", "built document Web directory")
	flag.Parse()
	if err := os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	app, err := document.Open(context.Background(), document.Config{Database: filepath.Join(*state, "document.db"), ContentDirectory: filepath.Join(*state, "content"), DocumentsDirectory: *directory})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		returned = errors.Join(returned, app.Close(ctx))
	}()
	if *id == "" {
		*id = randomToken()
	}
	var task document.Task
	switch *command {
	case "queue":
		task, _, err = app.Queue(context.Background(), *id, *path)
	case "convert":
		_, _, err = app.Queue(context.Background(), *id, *path)
		if err == nil {
			task, err = app.Run(context.Background(), *id)
		}
	case "run":
		task, err = app.Run(context.Background(), *id)
	case "status":
		task, err = app.Task(context.Background(), *id)
	case "serve":
		return serve(app, *state, *listen, *web)
	default:
		return fmt.Errorf("document: unknown command %q", *command)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(task); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
}

func serve(app *document.App, state, address, web string) error {
	tokenPath := filepath.Join(state, "application-token")
	token, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		token = []byte(randomToken())
		err = os.WriteFile(tokenPath, token, 0600)
	}
	if err != nil {
		return err
	}
	handler, err := app.Handler(document.HTTPConfig{ApplicationToken: strings.TrimSpace(string(token)), WebDirectory: web})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintf(os.Stderr, "Document workbench listening at http://%s; application token is in %s\n", listener.Addr(), tokenPath)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
func randomToken() string {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

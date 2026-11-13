package main

//go:generate go run . -command generate -out ../../examples/document/web/src/api.generated.ts
//go:generate node ../../node_modules/prettier/bin/prettier.cjs --write ../../examples/document/web/src/api.generated.ts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/internal/localfile"
)

type command string

const (
	commandConvert  command = "convert"
	commandQueue    command = "queue"
	commandRun      command = "run"
	commandStatus   command = "status"
	commandServe    command = "serve"
	commandGenerate command = "generate"
)

const (
	shutdownTimeout   = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, output io.Writer) error {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	flags := flag.NewFlagSet("document", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("command", string(commandConvert), "convert, queue, run, status, serve or generate")
	directory := flags.String("documents", ".", "document directory")
	state := flags.String("state", ".frame-document", "application state directory")
	path := flags.String("document", "", "relative document path")
	id := flags.String("task", "", "original task identity; generated for queue and convert when omitted")
	listen := flags.String("listen", "127.0.0.1:8088", "HTTP listen address")
	web := flags.String("web", "", "built document Web directory")
	projection := flags.String("out", "", "generated API path for the generate command")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("document: unexpected positional arguments")
	}
	selected := command(*mode)
	var task func(*document.App) (document.Task, error)
	switch selected {
	case commandGenerate:
		if *projection == "" {
			return errors.New("document: generate requires an output path")
		}
		return generateAPI(*projection)
	case commandServe:
		return withApplication(ctx, *state, *directory, func(app *document.App) error {
			return serve(ctx, app, *state, *listen, *web)
		})
	case commandQueue, commandConvert:
		if *id == "" {
			*id = rand.Text()
		}
		task = func(app *document.App) (document.Task, error) {
			queued, _, err := app.Queue(ctx, *id, *path)
			if err != nil || selected == commandQueue {
				return queued, err
			}
			return app.Run(ctx, *id)
		}
	case commandRun, commandStatus:
		if *id == "" {
			return errors.New("document: run and status require the original task identity")
		}
		task = func(app *document.App) (document.Task, error) {
			if selected == commandRun {
				return app.Run(ctx, *id)
			}
			return app.Task(ctx, *id)
		}
	default:
		return fmt.Errorf("document: unknown command %q", selected)
	}
	return withApplication(ctx, *state, *directory, func(app *document.App) error {
		result, err := task(app)
		return errors.Join(err, json.NewEncoder(output).Encode(result))
	})
}

func generateAPI(path string) error {
	var source bytes.Buffer
	if err := document.WriteTypeScriptAPI(&source); err != nil {
		return err
	}
	return localfile.WriteGenerated(path, source.Bytes())
}

func withApplication(ctx context.Context, state, directory string, work func(*document.App) error) (returned error) {
	if err := os.MkdirAll(state, localfile.PrivateDirectoryMode); err != nil {
		return err
	}
	app, err := document.Open(ctx, document.Config{Database: filepath.Join(state, "document.db"), ContentDirectory: filepath.Join(state, "content"), DocumentsDirectory: directory})
	if err != nil {
		return err
	}
	defer func() {
		wait, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		returned = errors.Join(returned, app.Close(wait))
	}()
	return work(app)
}

func serve(ctx context.Context, app *document.App, state, address, web string) error {
	tokenPath := filepath.Join(state, "application-token")
	token, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		token = []byte(applicationToken())
		err = os.WriteFile(tokenPath, token, localfile.PrivateFileMode)
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
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintf(os.Stderr, "Document workbench listening at http://%s; application token is in %s\n", listener.Addr(), tokenPath)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdown)
}

func applicationToken() string {
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	return hex.EncodeToString(secret[:])
}

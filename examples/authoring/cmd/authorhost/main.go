// Command authorhost is the trusted application fixture for the public
// TypeScript author exercise. It uses the real Host and browser control adapter.
package main

//go:generate go run . -generate ../../browser/src/contracts.generated.ts

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
	"reflect"
	"sync/atomic"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type AuthorPackage struct {
	Units []AuthorUnit `json:"units"`
}

// The author declares contracts and slots, while the application chooses the
// execution profile and provider policy. An empty exact Providers list must not
// be confused with an unresolved slot awaiting the Host's unique-provider rule.
type AuthorImport struct {
	Slot     string            `json:"slot"`
	Contract frame.ContractRef `json:"contract"`
	Optional bool              `json:"optional"`
}

type AuthorUnit struct {
	ID      frame.UnitID              `json:"id"`
	Imports []AuthorImport            `json:"imports"`
	Exports []frame.ExportDeclaration `json:"exports"`
}

type SourceConfiguration struct {
	DocumentBase string `json:"documentBase"`
}

type Bootstrap struct {
	Origin      string              `json:"origin"`
	Credentials browser.Credentials `json:"credentials"`
}

type moduleSource struct {
	release value.Digest
	digest  value.Digest
}

func (s moduleSource) Module(_ context.Context, binding frame.UnitBinding) (browser.Artifact, error) {
	if binding.Source.InstallationID != "author" || binding.Source.Release != s.release {
		return browser.Artifact{}, frame.ErrStaleBinding
	}
	return browser.Artifact{Path: "/plugin.mjs", Digest: s.digest}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (returned error) {
	generate := flag.String("generate", "", "write the document contract projection without starting a Host")
	state := flag.String("state", "", "empty, private state directory")
	packagePath := flag.String("package", "", "generated author package declaration")
	modulePath := flag.String("module", "", "built author ESM module")
	flag.Parse()
	contracts, err := document.NewContracts()
	if err != nil {
		return err
	}
	if *generate != "" {
		return writeContracts(*generate, contracts)
	}
	if *state == "" || *packagePath == "" || *modulePath == "" {
		return errors.New("authorhost: state, package and module are required")
	}
	declaration, err := readBounded(*packagePath, 1<<20)
	if err != nil {
		return err
	}
	module, err := readBounded(*modulePath, 16<<20)
	if err != nil {
		return err
	}
	var manifest AuthorPackage
	decoder := json.NewDecoder(bytes.NewReader(declaration))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("authorhost: declaration has trailing data")
	}
	units := make([]frame.Unit, len(manifest.Units))
	for index, unit := range manifest.Units {
		imports := make([]frame.ImportDeclaration, len(unit.Imports))
		for index, declared := range unit.Imports {
			imports[index] = frame.ImportDeclaration{Slot: declared.Slot, Contract: declared.Contract, Optional: declared.Optional}
		}
		units[index] = frame.Unit{Declaration: frame.UnitDeclaration{ID: unit.ID, Profile: frame.BrowserJS, Imports: imports, Exports: unit.Exports}}
	}
	// This trusted fixture reads immutable bytes before starting execution. It
	// does not claim remote package trust or define another distribution format.
	plugin, err := frame.Define(frame.Definition{Revision: value.Hash(append(append(bytes.Clone(declaration), 0), module...)), Units: units})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	admission := frame.NewAdmission()
	store, err := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(*state, "author.db"), Admission: admission})
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, store.Close()) }()
	host, err := frame.NewHost(context.Background(), frame.HostConfig{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		outcome, err := host.Close(ctx)
		if !outcome.Released {
			err = errors.Join(err, frame.ErrUnconfirmedRelease)
		}
		returned = errors.Join(returned, err)
	}()
	for _, contract := range []interface {
		Ref() frame.ContractRef
		Descriptor() []byte
	}{contracts.Files, contracts.Converters, contracts.Validators} {
		// The descriptor is the document contract owner. JavaScript objects stay
		// in their native Realm; they are not assigned a Go function type.
		if err := host.RegisterDescriptor(contract.Ref(), contract.Descriptor()); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: moduleSource{release: plugin.Revision(), digest: value.Hash(module)}, Points: []frame.ContractRef{contracts.Converters.Ref(), contracts.Validators.Ref()}, PublicConfiguration: func(context.Context, *frame.Setup) ([]byte, error) {
		return json.Marshal(SourceConfiguration{DocumentBase: origin + "/documents/"})
	}})
	if err != nil {
		return err
	}
	if err := host.RegisterDriver(bridge); err != nil {
		return err
	}
	if _, err := host.Install(context.Background(), frame.InstallationConfig{ID: "author", Plugin: plugin, Enabled: true}); err != nil {
		return err
	}
	credentials, err := bridge.OpenRealm(context.Background(), randomToken(), randomToken())
	if err != nil {
		return err
	}
	var started, cancelled atomic.Int64
	mux := http.NewServeMux()
	mux.Handle("POST /frame/control", bridge.Handler())
	mux.HandleFunc("GET /plugin.mjs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(module)
	})
	mux.HandleFunc("GET /documents/hello.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "# Public author\nOriginal source.\n")
	})
	mux.HandleFunc("GET /documents/untitled.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "A document without a title.\n")
	})
	mux.HandleFunc("GET /documents/wait.md", func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled.Add(1)
	})
	// Application operations require the original private Realm credential.
	// The adapter retains no independent execution or selection directory.
	application := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+credentials.Token {
				http.Error(w, "application credential required", http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /activate", application(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Unit frame.UnitID `json:"unit"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, err := host.ActivateUnit(r.Context(), browser.RealmID(credentials.Realm), frame.UnitRef{InstallationID: "author", Release: plugin.Revision(), UnitID: input.Unit})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /activity", application(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Started   int64 `json:"started"`
			Cancelled int64 `json:"cancelled"`
		}{started.Load(), cancelled.Load()})
	}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if err := json.NewEncoder(os.Stdout).Encode(Bootstrap{Origin: origin, Credentials: credentials}); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, closeErr := host.Close(closing)
	if !outcome.Released {
		return errors.Join(closeErr, frame.ErrUnconfirmedRelease)
	}
	return errors.Join(closeErr, server.Shutdown(closing))
}

func writeContracts(path string, contracts document.Contracts) error {
	var output bytes.Buffer
	if err := browser.WriteTypeScriptTypes(&output,
		reflect.TypeFor[document.Document](), reflect.TypeFor[document.Converted](),
		reflect.TypeFor[document.Diagnostic](), reflect.TypeFor[document.ConverterMetadata](),
		reflect.TypeFor[document.ValidatorMetadata](), reflect.TypeFor[AuthorPackage](),
		reflect.TypeFor[SourceConfiguration](), reflect.TypeFor[Bootstrap](),
	); err != nil {
		return err
	}
	refs, err := json.MarshalIndent(struct {
		Files      frame.ContractRef `json:"files"`
		Converters frame.ContractRef `json:"converters"`
		Validators frame.ContractRef `json:"validators"`
	}{contracts.Files.Ref(), contracts.Converters.Ref(), contracts.Validators.Ref()}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(&output, "\nexport const documentContracts = %s as const;\n", refs)
	return os.WriteFile(path, output.Bytes(), 0644)
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := localfile.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > maximum {
		return nil, errors.New("authorhost: input exceeds its byte budget")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if int64(len(data)) > maximum {
		return nil, errors.New("authorhost: input exceeds its byte budget")
	}
	return data, err
}

func randomToken() string {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

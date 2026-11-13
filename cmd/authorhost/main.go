// Command authorhost is the trusted application fixture for the public
// TypeScript author exercise. It uses the real Host and browser control adapter.
package main

//go:generate go run . -generate ../../examples/authoring/browser/src/contracts.generated.ts
//go:generate node ../../node_modules/prettier/bin/prettier.cjs --write ../../examples/authoring/browser/src/contracts.generated.ts

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
	framehost "github.com/Tangerg/frame/host"
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
	Slot     string                `json:"slot"`
	Contract framehost.ContractRef `json:"contract"`
	Optional bool                  `json:"optional"`
}

type AuthorUnit struct {
	ID      framehost.UnitID              `json:"id"`
	Imports []AuthorImport                `json:"imports"`
	Exports []framehost.ExportDeclaration `json:"exports"`
}

type SourceConfiguration struct {
	DocumentBase string `json:"documentBase"`
}

type Bootstrap struct {
	Origin      string              `json:"origin"`
	Credentials browser.Credentials `json:"credentials"`
}

type ActivationRequest struct {
	Unit framehost.UnitID `json:"unit"`
}

type ActivityView struct {
	Started   int64 `json:"started"`
	Cancelled int64 `json:"cancelled"`
}

type applicationPath string

const (
	controlPath   applicationPath = "/frame/control"
	activatePath  applicationPath = "/activate"
	activityPath  applicationPath = "/activity"
	sealRealmPath applicationPath = "/realm/seal"
)

type moduleSource struct {
	release value.Digest
	digest  value.Digest
}

func (s moduleSource) Module(_ context.Context, binding framehost.UnitBinding) (browser.Artifact, error) {
	if binding.Source.InstallationID != installationID || binding.Source.Release != s.release {
		return browser.Artifact{}, framehost.ErrStaleBinding
	}
	return browser.Artifact{Path: "/plugin.mjs", Digest: s.digest}, nil
}

const (
	installationID             = framehost.InstallationID("author")
	maxDeclarationSize         = 1 << 20
	maxModuleSize              = 16 << 20
	maxApplicationRequestBytes = 4096
	closeTimeout               = 10 * time.Second
	readHeaderTimeout          = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
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
	author, err := readAuthor(*packagePath, *modulePath)
	if err != nil {
		return err
	}
	return serve(*state, contracts, author)
}

// author is the immutable package under test: its declaration and module bytes
// together are the release identity.
type author struct {
	plugin *framehost.Plugin
	module []byte
}

func readAuthor(packagePath, modulePath string) (author, error) {
	declaration, err := readBounded(packagePath, maxDeclarationSize)
	if err != nil {
		return author{}, err
	}
	module, err := readBounded(modulePath, maxModuleSize)
	if err != nil {
		return author{}, err
	}
	var manifest AuthorPackage
	decoder := json.NewDecoder(bytes.NewReader(declaration))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return author{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return author{}, errors.New("authorhost: declaration has trailing data")
	}
	// This trusted fixture reads immutable bytes before starting execution. It
	// does not claim remote package trust or define another distribution format.
	plugin, err := framehost.Define(framehost.Definition{Revision: value.Hash(append(append(bytes.Clone(declaration), 0), module...)), Units: manifest.units()})
	return author{plugin: plugin, module: module}, err
}

func (p AuthorPackage) units() []framehost.Unit {
	units := make([]framehost.Unit, len(p.Units))
	for index, unit := range p.Units {
		imports := make([]framehost.ImportDeclaration, len(unit.Imports))
		for slot, declared := range unit.Imports {
			imports[slot] = framehost.ImportDeclaration{Slot: declared.Slot, Contract: declared.Contract, Optional: declared.Optional}
		}
		units[index] = framehost.Unit{Declaration: framehost.UnitDeclaration{ID: unit.ID, Profile: framehost.BrowserJS, Imports: imports, Exports: unit.Exports}}
	}
	return units
}

func serve(state string, contracts document.Contracts, author author) (returned error) {
	if err := os.MkdirAll(state, localfile.PrivateDirectoryMode); err != nil {
		return err
	}
	admission := framehost.NewAdmission()
	store, err := sqlite.Open(context.Background(), sqlite.Config{Path: filepath.Join(state, "author.db"), Admission: admission})
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, store.Close()) }()
	host, err := framehost.New(context.Background(), framehost.Config{Admission: admission, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store})
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, closeHost(host)) }()
	// The descriptor is the document contract owner. JavaScript objects stay
	// in their native Realm; they are not assigned a Go function type.
	for _, contract := range []framehost.Declaration{contracts.Files, contracts.Converters, contracts.Validators} {
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
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: moduleSource{release: author.plugin.Revision(), digest: value.Hash(author.module)}, Points: []framehost.ContractRef{contracts.Converters.Ref(), contracts.Validators.Ref()}, PublicConfiguration: func(context.Context, *framehost.Setup) ([]byte, error) {
		return json.Marshal(SourceConfiguration{DocumentBase: origin + "/documents/"})
	}})
	if err != nil {
		return err
	}
	if err := host.RegisterDriver(bridge); err != nil {
		return err
	}
	if _, err := host.Install(context.Background(), framehost.InstallationConfig{ID: installationID, Plugin: author.plugin, Enabled: true}); err != nil {
		return err
	}
	credentials, err := bridge.OpenRealm(context.Background(), rand.Text(), resumeToken())
	if err != nil {
		return err
	}
	server := &http.Server{Handler: routes(host, bridge, author, credentials), ReadHeaderTimeout: readHeaderTimeout}
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
	closing, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	return server.Shutdown(closing)
}

func closeHost(host framehost.Host) error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	outcome, err := host.Close(ctx)
	if !outcome.Released {
		err = errors.Join(err, frame.ErrUnconfirmedRelease)
	}
	return err
}

func routes(host framehost.Host, bridge *browser.Bridge, author author, credentials browser.Credentials) http.Handler {
	var activity activity
	mux := http.NewServeMux()
	mux.Handle(http.MethodPost+" "+string(controlPath), bridge.Handler())
	mux.HandleFunc("GET /plugin.mjs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(author.module)
	})
	mux.HandleFunc("GET /documents/hello.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "# Public author\nOriginal source.\n")
	})
	mux.HandleFunc("GET /documents/untitled.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "A document without a title.\n")
	})
	mux.HandleFunc("GET /documents/wait.md", activity.wait)
	application := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+credentials.Token {
				http.Error(w, "application credential required", http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc(activationHTTP.Method+" "+activationHTTP.Path, application(func(w http.ResponseWriter, r *http.Request) {
		activate(w, r, host, browser.RealmID(credentials.Realm), author.plugin.Revision())
	}))
	mux.HandleFunc(sealRealmHTTP.Method+" "+sealRealmHTTP.Path, application(func(w http.ResponseWriter, r *http.Request) {
		var input struct{}
		if !readApplicationRequest(w, r, &input) {
			return
		}
		if err := bridge.SealRealm(r.Context(), credentials.Token); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(sealRealmHTTP.SuccessStatus)
	}))
	mux.HandleFunc(activityHTTP.Method+" "+activityHTTP.Path, application(activity.report))
	return mux
}

type activity struct {
	started, cancelled atomic.Int64
}

func (a *activity) wait(w http.ResponseWriter, r *http.Request) {
	a.started.Add(1)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
	a.cancelled.Add(1)
}

func (a *activity) report(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(activityHTTP.SuccessStatus)
	_ = json.NewEncoder(w).Encode(ActivityView{a.started.Load(), a.cancelled.Load()})
}

func activate(w http.ResponseWriter, r *http.Request, host framehost.Host, realm framehost.RealmID, release value.Digest) {
	var input ActivationRequest
	if !readApplicationRequest(w, r, &input) {
		return
	}
	if _, err := host.ActivateUnit(r.Context(), realm, framehost.UnitRef{InstallationID: installationID, Release: release, UnitID: input.Unit}); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(activationHTTP.SuccessStatus)
}

func readApplicationRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApplicationRequestBytes))
	if err != nil {
		status := http.StatusBadRequest
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return false
	}
	if err := value.DecodeValue(data, target, "application request"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeContracts(path string, contracts document.Contracts) error {
	var output bytes.Buffer
	if err := browser.WriteTypeScriptHTTP(&output, []browser.HTTPOperation{activationHTTP, activityHTTP, sealRealmHTTP},
		reflect.TypeFor[document.Document](), reflect.TypeFor[document.Converted](),
		reflect.TypeFor[document.Diagnostic](), reflect.TypeFor[document.ConverterMetadata](),
		reflect.TypeFor[document.ValidatorMetadata](), reflect.TypeFor[AuthorPackage](),
		reflect.TypeFor[SourceConfiguration](), reflect.TypeFor[Bootstrap](),
	); err != nil {
		return err
	}
	refs, err := json.MarshalIndent(struct {
		Files      framehost.ContractRef `json:"files"`
		Converters framehost.ContractRef `json:"converters"`
		Validators framehost.ContractRef `json:"validators"`
	}{contracts.Files.Ref(), contracts.Converters.Ref(), contracts.Validators.Ref()}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(&output, "\nexport const documentContracts = %s as const;\n", refs)
	paths, err := json.MarshalIndent(struct {
		Control   applicationPath `json:"control"`
		Activate  applicationPath `json:"activate"`
		Activity  applicationPath `json:"activity"`
		SealRealm applicationPath `json:"sealRealm"`
	}{controlPath, activatePath, activityPath, sealRealmPath}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(&output, "\nexport const applicationPaths = %s as const;\n", paths)
	return localfile.WriteGenerated(path, output.Bytes())
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

func resumeToken() string {
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	return hex.EncodeToString(secret[:])
}

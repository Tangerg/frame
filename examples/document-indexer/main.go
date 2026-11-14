// Command document-indexer runs a zero-export Node plugin under an actual Frame Host.
package main

import (
	"context"
	_ "embed"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

//go:embed contracts.json
var domain []byte

type definition struct {
	Descriptor frame.CapabilityDescriptor `json:"descriptor"`
}

type fileRead struct {
	Path string `json:"path"`
}
type document struct {
	Path string `json:"path"`
	Text string `json:"text"`
}
type textResult struct {
	Text string `json:"text"`
}
type writeResult struct {
	Saved bool `json:"saved"`
}

const allowedDocument = "guide.md"
const documentText = "Frame indexes this document without an agent."

type documents struct{ indexed chan document }

func (d *documents) read(_ context.Context, invocation frame.Invocation) (value.JSON, error) {
	var request fileRead
	if err := value.DecodeValue(invocation.Input().Bytes(), &request, invocation.Member()); err != nil {
		return value.JSON{}, err
	}
	if request.Path != allowedDocument {
		return value.JSON{}, errors.New("document access denied")
	}
	return value.EncodeJSON(textResult{documentText})
}

func (d *documents) replace(ctx context.Context, invocation frame.Invocation) (value.JSON, error) {
	var request document
	if err := value.DecodeValue(invocation.Input().Bytes(), &request, invocation.Member()); err != nil {
		return value.JSON{}, err
	}
	if request.Path != allowedDocument {
		return value.JSON{}, errors.New("document access denied")
	}
	select {
	case d.indexed <- request:
	case <-ctx.Done():
		return value.JSON{}, frame.ContextError(ctx)
	}
	return value.EncodeJSON(writeResult{true})
}

func install(ctx context.Context, h framehost.Host, id framehost.InstallationID, revision value.Digest, u framehost.Unit) (framehost.UnitRef, error) {
	plugin, err := framehost.Define(framehost.Definition{Revision: revision, Units: []framehost.Unit{u}})
	if err != nil {
		return framehost.UnitRef{}, err
	}
	_, err = h.Install(ctx, framehost.InstallationConfig{ID: id, Plugin: plugin, Enabled: true})
	return framehost.UnitRef{InstallationID: id, Release: revision, UnitID: u.Declaration.ID}, err
}

func (d *documents) install(ctx context.Context, h framehost.Host, pluginRevision value.Digest) (framehost.UnitRef, error) {
	var definitions map[string]definition
	if err := json.Unmarshal(domain, &definitions); err != nil {
		return framehost.UnitRef{}, err
	}
	const hostID = "documents"
	host := framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "services", Profile: framehost.NativeGo}}
	child := framehost.Unit{Declaration: framehost.UnitDeclaration{ID: "document-indexer", Profile: framehost.LocalProcess}}
	hostRevision := value.Hash(domain)
	services := []struct {
		slot           string
		implementation frame.Service
	}{
		{"files", frame.Handler(d.read)},
		{"index", frame.Handler(d.replace)},
	}
	values := []framehost.ExportValue{}
	for _, service := range services {
		slot := service.slot
		spec, exists := definitions[slot]
		if !exists {
			return framehost.UnitRef{}, fmt.Errorf("missing contract for service %q", slot)
		}
		c, err := frame.AdmitCapability(spec.Descriptor)
		if err != nil {
			return framehost.UnitRef{}, err
		}
		contract, err := framehost.NewPortableContract[frame.Service]("1", c)
		if err != nil {
			return framehost.UnitRef{}, err
		}
		ref := contract.Ref()
		if err = h.Register(contract); err != nil {
			return framehost.UnitRef{}, err
		}
		host.Declaration.Exports = append(host.Declaration.Exports, framehost.ExportDeclaration{Name: slot, Contract: ref})
		values = append(values, framehost.Provide(slot, contract, service.implementation))
		provider := framehost.ExportRef{InstallationID: hostID, Release: hostRevision, UnitID: host.Declaration.ID, Export: slot, Contract: ref}
		child.Declaration.Imports = append(child.Declaration.Imports, framehost.ImportDeclaration{Slot: slot, Contract: ref})
		child.Declaration.Fixed = append(child.Declaration.Fixed, framehost.SlotProviders{Slot: slot, Providers: []framehost.ExportRef{provider}})
	}
	host.Setup = func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) { return values, nil }
	if _, err := install(ctx, h, hostID, hostRevision, host); err != nil {
		return framehost.UnitRef{}, err
	}
	return install(ctx, h, "indexer", pluginRevision, child)
}

type approvedProgram struct {
	program  process.Program
	revision value.Digest
}

func (p approvedProgram) Program(_ context.Context, binding framehost.UnitBinding) (process.Program, error) {
	if binding.Source.Release != p.revision {
		return process.Program{}, frame.ErrContractMismatch
	}
	return p.program, nil
}

func run(ctx context.Context, entry string) (answer string, returned error) {
	entry, err := filepath.Abs(entry)
	if err != nil {
		return "", err
	}
	plugin, err := os.ReadFile(entry)
	if err != nil {
		return "", err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return "", err
	}
	node, err = filepath.EvalSymlinks(node)
	if err != nil {
		return "", err
	}
	bin, err := os.ReadFile(node)
	if err != nil {
		return "", err
	}
	release, err := value.EncodeJSON(struct {
		Entry  string `json:"entry"`
		Domain string `json:"domain"`
	}{string(plugin), string(domain)})
	if err != nil {
		return "", err
	}
	revision := release.Digest()
	// This example trusts its local workspace. Pin the already-read script bytes
	// before dispatch so a later edit cannot change that entry's behavior.
	program := approvedProgram{process.Program{Command: node, ExecutableDigest: value.Hash(bin), Args: []string{"--input-type=module", "--eval", string(plugin)}, Directory: filepath.Dir(entry)}, revision}
	dir, err := os.MkdirTemp("", "frame-docs-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	server, err := runner.Open(runner.Config{Directory: filepath.Join(dir, "runner")})
	if err != nil {
		return "", err
	}
	defer func() { returned = errors.Join(returned, server.Close()) }()
	socket := filepath.Join(dir, "runner", "socket")
	listener, err := server.Listen(socket)
	if err != nil {
		return "", err
	}
	control, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(control, listener) }()
	defer func() {
		stop()
		closeErr := listener.Close()
		serveErr := <-served
		if errors.Is(serveErr, context.Canceled) {
			serveErr = nil
		}
		returned = errors.Join(returned, closeErr, serveErr)
	}()
	gate := framehost.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "state.db"), Admission: gate})
	if err != nil {
		return "", err
	}
	defer func() { returned = errors.Join(returned, store.Close()) }()
	client, err := runner.NewClient(socket)
	if err != nil {
		return "", err
	}
	driver, err := process.New(process.Config{Runner: client, Store: store, Source: program, CloseTimeout: closeTimeout, MaxFrameBytes: maxFrameBytes})
	if err != nil {
		return "", err
	}
	host, err := framehost.New(ctx, framehost.Config{Authority: store, Drivers: []framehost.Driver{driver}})
	if err != nil {
		return "", err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_, err := host.Close(closing)
		returned = errors.Join(returned, err)
	}()
	documents := &documents{indexed: make(chan document, 1)}
	root, err := documents.install(ctx, host, revision)
	if err != nil {
		return "", err
	}
	grant, err := gate.OpenScope(ctx, "document/"+allowedDocument)
	if err != nil {
		return "", err
	}
	running, err := host.StartUnit(ctx, grant, root)
	if err != nil {
		return "", err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		returned = errors.Join(returned, running.Stop(closing))
	}()
	select {
	case doc := <-documents.indexed:
		if doc.Text != documentText {
			return "", errors.New("index received unexpected document text")
		}
		answer = doc.Text
	case <-ctx.Done():
		return "", frame.ContextError(ctx)
	}
	return answer, nil
}

const (
	closeTimeout    = 5 * time.Second
	shutdownTimeout = 10 * time.Second
	runTimeout      = 30 * time.Second
	maxFrameBytes   = 1 << 20
)

func main() {
	entry := flag.String("entry", "examples/document-indexer/index.mjs", "trusted Node plugin entry")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	text, err := run(ctx, *entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(text)
}

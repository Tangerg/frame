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
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/process/runner"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

//go:embed contracts.json
var domain []byte

type definition struct {
	Descriptor portable.CapabilityDescriptor `json:"descriptor"`
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

func (d *documents) read(_ context.Context, _ string, input value.JSON) (value.JSON, error) {
	var request fileRead
	if err := value.DecodeValue(input.Bytes(), &request, "read"); err != nil {
		return value.JSON{}, err
	}
	if request.Path != allowedDocument {
		return value.JSON{}, errors.New("document access denied")
	}
	return value.EncodeJSON(textResult{documentText})
}

func (d *documents) replace(ctx context.Context, _ string, input value.JSON) (value.JSON, error) {
	var request document
	if err := value.DecodeValue(input.Bytes(), &request, "replace"); err != nil {
		return value.JSON{}, err
	}
	if request.Path != allowedDocument {
		return value.JSON{}, errors.New("document access denied")
	}
	select {
	case d.indexed <- request:
	case <-ctx.Done():
		return value.JSON{}, context.Cause(ctx)
	}
	return value.EncodeJSON(writeResult{true})
}

func install(ctx context.Context, h *frame.Host, id frame.InstallationID, revision value.Digest, u frame.Unit) (frame.UnitRef, error) {
	plugin, err := frame.Define(frame.Definition{Revision: revision, Units: []frame.Unit{u}})
	if err != nil {
		return frame.UnitRef{}, err
	}
	_, err = h.Install(ctx, frame.InstallationConfig{ID: id, Plugin: plugin, Enabled: true})
	return frame.UnitRef{InstallationID: id, Release: revision, UnitID: u.Declaration.ID}, err
}

func (d *documents) install(ctx context.Context, h *frame.Host, pluginRevision value.Digest) (frame.UnitRef, error) {
	var definitions map[string]definition
	if err := json.Unmarshal(domain, &definitions); err != nil {
		return frame.UnitRef{}, err
	}
	const hostID = "documents"
	host := frame.Unit{Declaration: frame.UnitDeclaration{ID: "services", Profile: frame.NativeGo}}
	child := frame.Unit{Declaration: frame.UnitDeclaration{ID: "document-indexer", Profile: frame.LocalProcess}}
	hostRevision := value.Hash(domain)
	services := map[string]process.Service{"files": process.Handler(d.read), "index": process.Handler(d.replace)}
	values := []frame.ExportValue{}
	for _, slot := range []string{"files", "index"} {
		spec := definitions[slot]
		c, err := portable.AdmitCapability(spec.Descriptor)
		if err != nil {
			return frame.UnitRef{}, err
		}
		contract, err := frame.NewPortableContract[process.Service]("1", c)
		if err != nil {
			return frame.UnitRef{}, err
		}
		ref := contract.Ref()
		if err = h.Register(contract); err != nil {
			return frame.UnitRef{}, err
		}
		host.Declaration.Exports = append(host.Declaration.Exports, frame.ExportDeclaration{Name: slot, Contract: ref})
		values = append(values, frame.Provide(slot, contract, services[slot]))
		provider := frame.ExportRef{InstallationID: hostID, Release: hostRevision, UnitID: host.Declaration.ID, Export: slot, Contract: ref}
		child.Declaration.Imports = append(child.Declaration.Imports, frame.ImportDeclaration{Slot: slot, Contract: ref, Providers: []frame.ExportRef{provider}})
	}
	host.Setup = func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return values, nil }
	if _, err := install(ctx, h, hostID, hostRevision, host); err != nil {
		return frame.UnitRef{}, err
	}
	return install(ctx, h, "indexer", pluginRevision, child)
}

type approvedProgram struct {
	program  process.Program
	revision value.Digest
}

func (p approvedProgram) Program(_ context.Context, binding frame.UnitBinding) (process.Program, error) {
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
	defer func() { stop(); _ = listener.Close(); <-served }()
	gate := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(dir, "state.db"), Admission: gate})
	if err != nil {
		return "", err
	}
	defer func() { returned = errors.Join(returned, store.Close()) }()
	driver, err := process.New(process.Config{Runner: runner.Dial(socket), Store: store, Source: program, CloseTimeout: 5 * time.Second, MaxFrameBytes: 1 << 20})
	if err != nil {
		return "", err
	}
	host, err := frame.NewHost(ctx, frame.HostConfig{Admission: gate, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store, Drivers: []frame.Driver{driver}})
	if err != nil {
		return "", err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		return "", context.Cause(ctx)
	}
	return answer, nil
}

func main() {
	entry := flag.String("entry", "examples/document-indexer/index.mjs", "trusted Node plugin entry")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	text, err := run(ctx, *entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(text)
}

package mcp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const (
	managedCallback           = managedNamespace + "callback.v1"
	managedCallbackTokenBytes = 32
)

// ManagedCallback is a host-issued lease on one authenticated loopback route.
// Revoke must invalidate that exact credential and call token, including in-flight users.
type ManagedCallback struct {
	Endpoint         string          `json:"endpoint"`
	Credential       string          `json:"credential"`
	Call             string          `json:"call"`
	MaxResponseBytes int64           `json:"maxResponseBytes"`
	Released         <-chan struct{} `json:"-"`
	Revoke           func() error    `json:"-"`
}

func (g ManagedCallback) attach(metadata *sdk.ParamsBase, key string) error {
	endpoint, err := url.Parse(g.Endpoint)
	if err != nil || endpoint.Scheme != callbackScheme || endpoint.Hostname() != loopbackHost || endpoint.Port() == "" || endpoint.Path != "/" || endpoint.RawQuery != "" || endpoint.User != nil || endpoint.Fragment != "" || g.MaxResponseBytes <= 0 || g.MaxResponseBytes == math.MaxInt64 || g.Revoke == nil || g.Released == nil {
		return errors.New("mcp: invalid managed callback lease")
	}
	for _, token := range []string{g.Credential, g.Call} {
		if len(token) != hex.EncodedLen(managedCallbackTokenBytes) {
			return errors.New("mcp: invalid managed callback credential")
		}
		if _, err := hex.DecodeString(token); err != nil {
			return errors.New("mcp: invalid managed callback credential")
		}
	}
	return metadata.Set(key, g)
}

// OpenManagedCallbacks creates the host's route for one approved setup. The
// host authenticates every callback against this session and its frozen imports.
type OpenManagedCallbacks func(context.Context, *framehost.Setup, *sdk.ClientSession) (ManagedCallback, error)

// ManagedCall carries the already-admitted request to the host that decides
// reverse permissions. The physical backend cannot replace any of this evidence.
type ManagedCall struct {
	Source    framehost.ExportRef
	Method    *frame.Method
	Input     value.JSON
	CommandID string
}

// OpenManagedCall optionally issues a business callback for one invocation.
// A nil lease grants nothing. Its host owns the permitted effects and target.
type OpenManagedCall func(context.Context, *sdk.ClientSession, ManagedCall) (*ManagedCallback, error)

// ManagedImport relates a backend dependency name to a resolved Frame slot.
// The provider set and optionality come exclusively from Frame's binding.
type ManagedImport struct {
	ID       string
	Slot     string
	Contract *ManagedContract
}

type managedDependency struct {
	ID       string           `json:"id"`
	Contract value.JSON       `json:"contract"`
	Optional bool             `json:"optional"`
	Bindings []managedBinding `json:"bindings"`
}
type managedBinding struct {
	InstallationID framehost.InstallationID `json:"installationId"`
	Digest         value.Digest             `json:"digest"`
	Contribution   string                   `json:"contribution"`
	Contract       value.JSON               `json:"contract"`
}

type managedAddress struct {
	installation framehost.InstallationID
	release      value.Digest
	contribution string
}

type managedRoute struct {
	source   framehost.ExportRef
	contract value.JSON
}

type managedRoutes map[managedAddress]managedRoute

func (routes managedRoutes) bind(source framehost.ExportRef, contract value.JSON) (managedBinding, error) {
	key := managedAddress{source.InstallationID, source.Release, source.Export}
	route := managedRoute{source, contract}
	if existing, found := routes[key]; found && existing != route {
		return managedBinding{}, fmt.Errorf("%w: Qimu callback address cannot distinguish its Frame providers", frame.ErrContractMismatch)
	}
	routes[key] = route
	return managedBinding{source.InstallationID, source.Release, source.Export, contract}, nil
}

func projectDependencies(setup *framehost.Setup, imports map[string]framehost.ImportDeclaration, used map[string]bool, routes managedRoutes, declared []ManagedImport) ([]managedDependency, error) {
	dependencies := make([]managedDependency, 0, len(declared))
	names := make(map[string]bool, len(declared))
	for _, item := range declared {
		slot, found := imports[item.Slot]
		if !found || item.Contract == nil || item.Contract.capability == nil || framehost.UnitID(item.ID).Validate() != nil || names[item.ID] {
			return nil, fmt.Errorf("%w: invalid managed dependency %q", frame.ErrContractMismatch, item.ID)
		}
		names[item.ID] = true
		used[item.Slot] = true
		contract, err := setup.Capability(slot.Contract)
		if err != nil {
			return nil, err
		}
		if contract.Identity() != item.Contract.capability.Identity() {
			return nil, frame.ErrContractMismatch
		}
		resolved, err := setup.Imports(item.Slot)
		if err != nil {
			return nil, err
		}
		dep := managedDependency{ID: item.ID, Optional: slot.Optional, Contract: item.Contract.wire, Bindings: make([]managedBinding, 0, len(resolved))}
		for _, provider := range resolved {
			binding, err := routes.bind(provider.Source, item.Contract.wire)
			if err != nil {
				return nil, err
			}
			dep.Bindings = append(dep.Bindings, binding)
		}
		dependencies = append(dependencies, dep)
	}
	return dependencies, nil
}

// installationTarget addresses the installation a background contribution
// runs for, independently of any foreground Use.
func installationTarget(setup *framehost.Setup) *frame.Target {
	return &frame.Target{Scope: installationTargetScope, Reference: setup.Binding().Source.InstallationID.String()}
}

const installationTargetScope = "installation"

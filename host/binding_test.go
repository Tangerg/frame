package host_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

func TestDefinitionValidatesExplicitProviders(t *testing.T) {
	service, _ := framehost.NewContract[string]("example.explicit", "1")
	point, _ := framehost.NewPoint[string, string]("example.explicit.point", "1")
	provider := framehost.ExportRef{InstallationID: "provider", Release: value.Hash([]byte("provider")), UnitID: "main", Export: "value", Contract: service.Ref()}
	other := provider
	other.Export = "other"
	member := provider
	member.Contract = point.Ref()
	otherMember := member
	otherMember.Export = "other"
	fixed := func(slot string, providers ...framehost.ExportRef) []framehost.SlotProviders {
		return []framehost.SlotProviders{{Slot: slot, Providers: providers}}
	}
	for _, test := range []struct {
		name     string
		contract framehost.ContractRef
		optional bool
		fixed    []framehost.SlotProviders
		valid    bool
	}{
		{name: "unresolved", contract: service.Ref(), valid: true},
		{name: "required empty", contract: service.Ref(), fixed: fixed("input")},
		{name: "optional empty", contract: service.Ref(), optional: true, fixed: fixed("input"), valid: true},
		{name: "service", contract: service.Ref(), fixed: fixed("input", provider), valid: true},
		{name: "service multiple", contract: service.Ref(), fixed: fixed("input", provider, other)},
		{name: "service duplicate", contract: service.Ref(), fixed: fixed("input", provider, provider)},
		{name: "point duplicate", contract: point.Ref(), fixed: fixed("input", member, member)},
		{name: "point multiple", contract: point.Ref(), fixed: fixed("input", member, otherMember), valid: true},
		{name: "undeclared slot", contract: service.Ref(), fixed: fixed("other", provider)},
		{name: "repeated slot", contract: point.Ref(), fixed: append(fixed("input", member), fixed("input", otherMember)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			plugin, err := framehost.Define(framehost.Definition{
				Revision: value.Hash([]byte(test.name)),
				Units: []framehost.Unit{{
					Declaration: framehost.UnitDeclaration{ID: "consumer", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{Slot: "input", Contract: test.contract, Optional: test.optional}}, Fixed: test.fixed},
					Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
						t.Fatal("definition started plugin code")
						return nil, nil
					},
				}},
			})
			if test.valid {
				if plugin == nil || err != nil {
					t.Fatalf("valid import rejected: %v", err)
				}
			} else if plugin != nil || !errors.Is(err, frame.ErrInvalid) {
				t.Fatalf("invalid explicit import constructed: plugin=%v, err=%v", plugin, err)
			}
		})
	}
}

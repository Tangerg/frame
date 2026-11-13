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
	for _, test := range []struct {
		name      string
		contract  framehost.ContractRef
		optional  bool
		providers []framehost.ExportRef
		valid     bool
	}{
		{name: "unresolved", contract: service.Ref(), valid: true},
		{name: "required empty", contract: service.Ref(), providers: []framehost.ExportRef{}},
		{name: "optional empty", contract: service.Ref(), optional: true, providers: []framehost.ExportRef{}, valid: true},
		{name: "service", contract: service.Ref(), providers: []framehost.ExportRef{provider}, valid: true},
		{name: "service multiple", contract: service.Ref(), providers: []framehost.ExportRef{provider, other}},
		{name: "service duplicate", contract: service.Ref(), providers: []framehost.ExportRef{provider, provider}},
		{name: "point duplicate", contract: point.Ref(), providers: []framehost.ExportRef{member, member}},
		{name: "point multiple", contract: point.Ref(), providers: []framehost.ExportRef{member, otherMember}, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plugin, err := framehost.Define(framehost.Definition{
				Revision: value.Hash([]byte(test.name)),
				Units: []framehost.Unit{{
					Declaration: framehost.UnitDeclaration{ID: "consumer", Profile: framehost.NativeGo, Imports: []framehost.ImportDeclaration{{
						Slot: "input", Contract: test.contract, Optional: test.optional, Providers: test.providers,
					}}},
					Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
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

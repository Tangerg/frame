package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestDefinitionValidatesExplicitProviders(t *testing.T) {
	service, _ := frame.NewContract[string]("example.explicit", "1")
	point, _ := frame.NewPoint[string, string]("example.explicit.point", "1")
	provider := frame.ExportRef{InstallationID: "provider", Release: value.Hash([]byte("provider")), UnitID: "main", Export: "value", Contract: service.Ref()}
	other := provider
	other.Export = "other"
	member := provider
	member.Contract = point.Ref()
	otherMember := member
	otherMember.Export = "other"
	for _, test := range []struct {
		name      string
		contract  frame.ContractRef
		optional  bool
		providers []frame.ExportRef
		valid     bool
	}{
		{name: "unresolved", contract: service.Ref(), valid: true},
		{name: "required empty", contract: service.Ref(), providers: []frame.ExportRef{}},
		{name: "optional empty", contract: service.Ref(), optional: true, providers: []frame.ExportRef{}, valid: true},
		{name: "service", contract: service.Ref(), providers: []frame.ExportRef{provider}, valid: true},
		{name: "service multiple", contract: service.Ref(), providers: []frame.ExportRef{provider, other}},
		{name: "service duplicate", contract: service.Ref(), providers: []frame.ExportRef{provider, provider}},
		{name: "point duplicate", contract: point.Ref(), providers: []frame.ExportRef{member, member}},
		{name: "point multiple", contract: point.Ref(), providers: []frame.ExportRef{member, otherMember}, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plugin, err := frame.Define(frame.Definition{
				Revision: value.Hash([]byte(test.name)),
				Units: []frame.Unit{{
					Declaration: frame.UnitDeclaration{ID: "consumer", Profile: frame.NativeGo, Imports: []frame.ImportDeclaration{{
						Slot: "input", Contract: test.contract, Optional: test.optional, Providers: test.providers,
					}}},
					Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
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

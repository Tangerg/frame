// Build in the upstream Flame Go workspace. This peer links Qimu and never imports Frame.
package main

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"iter"
	"os"

	sdk "github.com/Tangerg/go-sdk/mcp"
	qimu "github.com/Tangerg/qimu/mcp"
	"github.com/Tangerg/qimu/portable"
)

func run() error {
	method, err := qimu.NewStreamingMethod[string, int, bool](qimu.MethodConfig{Name: "records.read", Kind: portable.Mutation, Target: "collection", Description: "Read two ordered records."})
	if err != nil {
		return err
	}
	contract, err := portable.NewCapability(portable.CapabilityConfig{Reference: "records.v1", Composition: portable.Selection, Scope: "installation"}, method.Descriptor())
	if err != nil {
		return err
	}
	if len(os.Args) == 2 && os.Args[1] == "descriptor" {
		data, err := json.Marshal(contract.Descriptor())
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "upstream-stream", Version: "1"}, nil)
	backend, err := qimu.NewBackend(server)
	if err != nil {
		return err
	}
	err = method.RegisterServer(backend, func(_ context.Context, _ *sdk.ServerSession, command qimu.Command[string]) iter.Seq2[qimu.StreamValue[int, bool], error] {
		return func(yield func(qimu.StreamValue[int, bool], error) bool) {
			if command.ID != "original-stream" || command.Input != "two" {
				yield(qimu.StreamValue[int, bool]{}, fmt.Errorf("unexpected command identity or input"))
				return
			}
			for _, n := range []int{1, 2} {
				if !yield(qimu.StreamValue[int, bool]{Chunk: &n}, nil) {
					return
				}
			}
			yield(qimu.StreamValue[int, bool]{Result: new(true)}, nil)
		}
	})
	if err != nil {
		return err
	}
	if err := backend.BindCapability("records", contract); err != nil {
		return err
	}
	if err := backend.Publish(); err != nil {
		return err
	}
	return server.Run(context.Background(), &sdk.StdioTransport{})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Tangerg/frame/examples/buildcheck"
)

func main() {
	apply := flag.Bool("apply", false, "apply the selected formatter to the captured revision")
	flag.Parse()
	if err := run(*apply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const maxInputBytes = 4 << 20

func run(apply bool) error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxInputBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxInputBytes {
		return fmt.Errorf("buildcheck: input exceeds %d bytes", maxInputBytes)
	}
	source := buildcheck.NewSource(string(data))
	engine, err := buildcheck.New(context.Background(), source)
	if err != nil {
		return err
	}
	defer engine.Close(context.Background())
	result, err := engine.Run(context.Background())
	if err != nil {
		return err
	}
	if apply {
		if _, err := source.Apply(result.Patch); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

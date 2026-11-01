package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/control"
)

func main() {
	output := flag.String("out", "", "typescript output path")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "controlgen: -out is required")
		os.Exit(2)
	}
	var data bytes.Buffer
	if err := control.WriteTypeScript(&data); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*output, data.Bytes(), 0644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

package main

//go:generate go run . -out ../../web/src/api.generated.ts
//go:generate node ../../../../node_modules/prettier/bin/prettier.cjs --write ../../web/src/api.generated.ts

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/Tangerg/frame/examples/document"
	"github.com/Tangerg/frame/internal/localfile"
)

func main() {
	output := flag.String("out", "web/src/api.generated.ts", "generated domain DTO path")
	flag.Parse()
	var source bytes.Buffer
	err := document.WriteTypeScriptAPI(&source)
	if err == nil {
		err = localfile.WriteGenerated(*output, source.Bytes())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

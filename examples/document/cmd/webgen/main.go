package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/Tangerg/frame/control"
	controlhost "github.com/Tangerg/frame/control/host"
	"github.com/Tangerg/frame/examples/document"
)

func main() {
	output := flag.String("out", "web/src/api.generated.ts", "generated domain DTO path")
	flag.Parse()
	file, err := os.Create(*output)
	if err == nil {
		err = control.WriteTypeScriptTypes(file, reflect.TypeFor[document.TaskView](), reflect.TypeFor[document.PreviewMetadata](), reflect.TypeFor[controlhost.Credentials]())
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

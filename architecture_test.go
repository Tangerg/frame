package frame_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/Tangerg/frame"

func TestFrameworkSourceTestsAndCommandsDoNotImportConsumers(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git" || entry.Name() == "testdata" || entry.Name() == "dist") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			name := strings.Trim(imported.Path.Value, `"`)
			dir := filepath.ToSlash(filepath.Dir(path))
			mcpSDK := strings.HasPrefix(name, "github.com/modelcontextprotocol/") || strings.HasPrefix(name, "github.com/Tangerg/go-sdk/")
			switch {
			case name == "github.com/Tangerg/flame" || strings.HasPrefix(name, "github.com/Tangerg/flame/") || name == "github.com/Tangerg/qimu" || strings.HasPrefix(name, "github.com/Tangerg/qimu/"):
				t.Errorf("%s imports the host or its former framework %s", path, name)
			case mcpSDK && dir != "process/mcp":
				t.Errorf("%s imports protocol SDK %s outside its adapter", path, name)
			case name == modulePath+"/process/mcp" && !strings.HasPrefix(dir, "examples/") && dir != "process/mcp" && !strings.HasPrefix(dir, "conformance"):
				t.Errorf("%s makes the optional MCP adapter a dependency", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPortableValuesCannotImportHostOrExecutionCapabilities(t *testing.T) {
	for _, directory := range []string{"portable", "value"} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				name := strings.Trim(imported.Path.Value, `"`)
				frameworkCapability := name == modulePath || strings.HasPrefix(name, modulePath+"/") && name != modulePath+"/portable" && name != modulePath+"/value"
				externalCapability := name == "os" || name == "io/fs" || name == "database/sql" || name == "net" || strings.HasPrefix(name, "net/") && name != "net/url" && name != "net/netip"
				if frameworkCapability || externalCapability {
					t.Errorf("pure value %s imports capability %s", path, name)
				}
			}
		}
	}
}

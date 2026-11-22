package value

import (
	"fmt"
	"strings"
)

// DefinitionPointer is the JSON Pointer prefix of a schema's local definitions.
const DefinitionPointer = "/" + KeywordDefs + "/"

// JSON Pointer (RFC 6901) escapes "~" before "/", so a member name can never
// address another path and unescaping restores exactly the original name.
var (
	pointerEscaper   = strings.NewReplacer("~", "~0", "/", "~1")
	pointerUnescaper = strings.NewReplacer("~1", "/", "~0", "~")
)

// PointerToken spells one member name as a JSON Pointer reference token.
func PointerToken(name string) string { return pointerEscaper.Replace(name) }

// PointerName reads the member name one JSON Pointer reference token spells.
func PointerName(token string) string { return pointerUnescaper.Replace(token) }

// PointerNames splits a JSON Pointer into the member names it addresses; the
// empty pointer addresses the whole document.
func PointerNames(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("value: invalid JSON pointer %q", pointer)
	}
	names := strings.Split(pointer[1:], "/")
	for i, token := range names {
		names[i] = PointerName(token)
	}
	return names, nil
}

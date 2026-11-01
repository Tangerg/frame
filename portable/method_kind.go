package portable

import "fmt"

func MethodKindFor(encoded []byte, name string) (MethodKind, error) {
	contract, err := ParseCapability(encoded)
	if err != nil {
		return "", err
	}
	method, found := contract.Method(name)
	if !found {
		return "", fmt.Errorf("capability: unknown method %q", name)
	}
	return method.Kind(), nil
}

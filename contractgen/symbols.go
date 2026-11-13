package contractgen

import (
	"strconv"
)

type symbolTable struct{ claimed map[string]struct{} }

func newSymbolTable(reserved ...string) *symbolTable {
	symbols := &symbolTable{claimed: make(map[string]struct{}, len(reserved))}
	for _, name := range reserved {
		symbols.claimed[name] = struct{}{}
	}
	return symbols
}

func (s *symbolTable) claim(preferred string) string {
	return s.claimFamily(preferred, "")
}

func (s *symbolTable) claimFamily(preferred string, members ...string) string {
	name := preferred
	for suffix := 2; ; suffix++ {
		available := true
		for _, member := range members {
			if _, exists := s.claimed[name+member]; exists {
				available = false
				break
			}
		}
		if available {
			for _, member := range members {
				s.claimed[name+member] = struct{}{}
			}
			return name
		}
		name = preferred + strconv.Itoa(suffix)
	}
}

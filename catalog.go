package frame

import (
	"cmp"
	"fmt"
	"slices"
)

// publication is one visible member of a contract's catalog. Only the catalog
// changes its visibility and revision.
type publication struct {
	id       string
	source   ExportRef
	metadata any
	value    any
	instance *Instance
	durable  bool
	visible  bool
	revision uint64
}

type contractMembers struct {
	revision uint64
	members  map[string]*publication
}

// catalog owns live membership per contract. Every change advances that
// contract's revision and is delivered to its Watches; Host.mu guards it.
type catalog struct {
	contracts       map[ContractRef]*contractMembers
	watches         map[*catalogWatch]struct{}
	maxPublications int
}

func newCatalog(maxPublications int) catalog {
	return catalog{contracts: make(map[ContractRef]*contractMembers), watches: make(map[*catalogWatch]struct{}), maxPublications: maxPublications}
}

func (c *catalog) count() int {
	count := 0
	for _, contract := range c.contracts {
		count += len(contract.members)
	}
	return count
}

func (c *catalog) reserve(publications int) error {
	if c.count()+publications > c.maxPublications {
		return fmt.Errorf("%w: publications", ErrCapacity)
	}
	return nil
}

func (c *catalog) revision(contract ContractRef) uint64 {
	if members := c.contracts[contract]; members != nil {
		return members.revision
	}
	return 0
}

func (c *catalog) member(contract ContractRef, id string) *publication {
	if members := c.contracts[contract]; members != nil {
		return members.members[id]
	}
	return nil
}

func (c *catalog) find(id string) *publication {
	for _, members := range c.contracts {
		if value := members.members[id]; value != nil {
			return value
		}
	}
	return nil
}

func (c *catalog) members(contract ContractRef) []*publication {
	members := c.contracts[contract]
	if members == nil {
		return nil
	}
	result := make([]*publication, 0, len(members.members))
	for _, value := range members.members {
		result = append(result, value)
	}
	slices.SortFunc(result, func(a, b *publication) int {
		return cmp.Or(compareExport(a.source, b.source), cmp.Compare(a.id, b.id))
	})
	return result
}

func (c *catalog) publish(value *publication) {
	members := c.contracts[value.source.Contract]
	if members == nil {
		members = &contractMembers{members: make(map[string]*publication)}
		c.contracts[value.source.Contract] = members
	}
	members.members[value.id] = value
	value.visible = true
	members.revision++
	value.revision = members.revision
	c.notify(value.source.Contract, members.revision)
}

func (c *catalog) withdraw(value *publication) {
	if !value.visible {
		return
	}
	value.visible = false
	members := c.contracts[value.source.Contract]
	delete(members.members, value.id)
	members.revision++
	c.notify(value.source.Contract, members.revision)
}

func (c *catalog) notify(contract ContractRef, revision uint64) {
	for watch := range c.watches {
		if slices.Contains(watch.contracts, contract) {
			watch.deliverLocked(Change{Contract: contract, Revision: revision})
		}
	}
}

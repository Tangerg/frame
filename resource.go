package frame

import (
	"context"
	"fmt"
)

type ReleaseState string

const (
	ResourceReleased    ReleaseState = "released"
	ResourceUnconfirmed ReleaseState = "unconfirmed"
)

func (ReleaseState) JSONSchemaEnum() []string {
	return []string{string(ResourceReleased), string(ResourceUnconfirmed)}
}

// Resource describes actual owned work. Cancel must only request cancellation;
// Close supplies the release conclusion. An error alone never proves release.
type Resource struct {
	Name   string
	Cancel func(error)
	Close  func(context.Context) (ReleaseState, error)
}

// cleanupState is the progress of a resource's own Close callback. Release
// proof is a separate fact: it may arrive before, during or after cleanup.
type cleanupState uint8

const (
	cleanupPending cleanupState = iota
	cleanupRunning
	cleanupFinished
)

// resourceEntry is a node of the release graph: it closes only after every
// consumer that depends on it has been released.
type resourceEntry struct {
	resource     Resource
	dependencies []*resourceEntry
	consumers    int
	index        int
	cleanup      cleanupState
	released     bool
}

// ResourceHandle reports evidence to the original resource owner. It cannot
// restart cleanup or transfer release ownership to a different lifetime. The
// zero handle is unissued.
type ResourceHandle struct {
	lifetime *lifetimeRecord
	entry    *resourceEntry
}

func (h ResourceHandle) OwnerID() LifetimeID { return LifetimeID{h.lifetime} }

func (h ResourceHandle) Valid() bool { return h.entry != nil }

func (l *lifetimeRecord) Own(resource Resource, dependencies ...ResourceHandle) (ResourceHandle, error) {
	if l == nil {
		return ResourceHandle{}, ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closingLocked() {
		return ResourceHandle{}, ErrClosed
	}
	return l.ownLocked(resource, dependencies)
}

func (l *lifetimeRecord) ownLocked(resource Resource, dependencies []ResourceHandle) (ResourceHandle, error) {
	if resource.Name == "" || resource.Close == nil {
		return ResourceHandle{}, ErrInvalid
	}
	if len(l.resources) >= l.limits.MaxResources {
		return ResourceHandle{}, fmt.Errorf("%w: lifetime resources", ErrCapacity)
	}
	resource.Name = BoundedDiagnostic(resource.Name)
	entry := &resourceEntry{resource: resource, index: len(l.resources)}
	seen := make(map[*resourceEntry]bool)
	for _, dependency := range dependencies {
		if !dependency.Valid() || dependency.lifetime != l || dependency.entry.released {
			return ResourceHandle{}, fmt.Errorf("%w: resource dependency", ErrInvalid)
		}
		if seen[dependency.entry] {
			continue
		}
		seen[dependency.entry] = true
		entry.dependencies = append(entry.dependencies, dependency.entry)
	}
	for _, dependency := range entry.dependencies {
		dependency.consumers++
	}
	l.resources = append(l.resources, entry)
	l.signalLocked()
	return ResourceHandle{lifetime: l, entry: entry}, nil
}

func cleanupResource(name string, closeResource func() error) Resource {
	resource := Resource{Name: name}
	if closeResource != nil {
		resource.Close = func(context.Context) (ReleaseState, error) {
			if err := closeResource(); err != nil {
				return ResourceUnconfirmed, err
			}
			return ResourceReleased, nil
		}
	}
	return resource
}

func (l *lifetimeRecord) Cleanup(name string, closeResource func() error, dependencies ...ResourceHandle) (ResourceHandle, error) {
	return l.Own(cleanupResource(name, closeResource), dependencies...)
}

func (h ResourceHandle) ConfirmReleased(diagnostic error) error {
	if !h.Valid() {
		return ErrInvalid
	}
	detail := closeFailureDetail(diagnostic)
	l := h.lifetime
	l.mu.Lock()
	defer l.mu.Unlock()
	if h.entry.released {
		return nil
	}
	h.entry.released = true
	if diagnostic != nil {
		l.recordFailureLocked(h.entry.resource.Name, detail, diagnostic)
	}
	if h.entry.cleanup != cleanupRunning {
		l.forgetResourceLocked(h.entry)
	}
	l.signalLocked()
	return nil
}

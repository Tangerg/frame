package frame

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
)

// EventRoute is an opaque authorization to communicate between two exact
// objects in already admitted Uses. It contains no handler or native payload.
type EventRoute struct{ identity *eventRouteIdentity }

type eventRouteIdentity struct {
	id        string
	authority *authorityIdentity
	source    ExportRef
	receiver  ExportRef
}

func (r *EventRoute) ID() string {
	if r == nil || r.identity == nil {
		return ""
	}
	return r.identity.id
}
func (r *EventRoute) Source() ExportRef {
	if r == nil || r.identity == nil {
		return ExportRef{}
	}
	return r.identity.source
}
func (r *EventRoute) Receiver() ExportRef {
	if r == nil || r.identity == nil {
		return ExportRef{}
	}
	return r.identity.receiver
}

// eventRoute is live while the Host indexes it. It is a resource of both Uses,
// so closing either one ends the route.
type eventRoute struct {
	identity         *eventRouteIdentity
	source           *Use
	receiver         *Use
	sourceResource   *ResourceHandle
	receiverResource *ResourceHandle
}

const eventRoutePrefix = "event route:"

func (u *Use) exports(source ExportRef) bool {
	for _, member := range u.members {
		if member.source == source {
			return true
		}
	}
	return false
}

func (h *Host) NewEventRoute(ctx context.Context, sourceUse, receiverUse *Use, source, receiver ExportRef) (*EventRoute, error) {
	if sourceUse == nil || receiverUse == nil || sourceUse.authority != h.identity || receiverUse.authority != h.identity {
		return nil, ErrInvalid
	}
	if source.Contract.Kind != EventKind || source.Contract != receiver.Contract {
		return nil, ErrContractMismatch
	}
	route := &eventRoute{identity: &eventRouteIdentity{id: rand.Text(), authority: h.identity, source: source, receiver: receiver}, source: sourceUse, receiver: receiverUse}
	err := h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.admitRouteLocked(route); err != nil {
			return err
		}
		return h.ownRouteLocked(route)
	})
	if err != nil {
		return nil, err
	}
	return &EventRoute{identity: route.identity}, nil
}

func (h *Host) admitRouteLocked(route *eventRoute) error {
	if err := h.validateRouteUsesLocked(route); err != nil {
		return err
	}
	if !route.source.exports(route.identity.source) || !route.receiver.exports(route.identity.receiver) {
		return ErrContractMismatch
	}
	if route.source.realm != route.receiver.realm && route.identity.source.Contract.Mode == NativeMode {
		return ErrContractMismatch
	}
	if len(h.routes) >= h.limits.MaxEventRoutes {
		return fmt.Errorf("%w: event routes", ErrCapacity)
	}
	return nil
}

func (h *Host) ownRouteLocked(route *eventRoute) error {
	resource := Resource{Name: eventRoutePrefix + route.identity.id, Close: func(context.Context) (ReleaseState, error) {
		h.releaseEventRoute(route)
		return ResourceReleased, nil
	}}
	sourceResource, err := route.source.lifetime.Own(resource)
	if err != nil {
		return err
	}
	route.sourceResource = sourceResource
	if route.receiver != route.source {
		receiverResource, err := route.receiver.lifetime.Own(resource)
		if err != nil {
			_ = sourceResource.ConfirmReleased(nil)
			return err
		}
		route.receiverResource = receiverResource
	}
	h.routes[route.identity.id] = route
	return nil
}

func (h *Host) releaseEventRoute(route *eventRoute) {
	h.mu.Lock()
	if h.routes[route.identity.id] == route {
		delete(h.routes, route.identity.id)
	}
	h.mu.Unlock()
	for _, resource := range []*ResourceHandle{route.sourceResource, route.receiverResource} {
		if resource != nil {
			_ = resource.ConfirmReleased(nil)
		}
	}
}

func (h *Host) validateRouteUsesLocked(route *eventRoute) error {
	if h.lifetime.isClosing() || h.uses[route.source.id] != route.source || h.uses[route.receiver.id] != route.receiver {
		return ErrClosed
	}
	if err := h.admission.ValidateScope(route.source.scope); err != nil {
		return err
	}
	return h.admission.ValidateScope(route.receiver.scope)
}

func (h *Host) liveRouteLocked(token *EventRoute) (*eventRoute, error) {
	if token == nil || token.identity == nil || token.identity.authority != h.identity {
		return nil, ErrInvalid
	}
	route := h.routes[token.identity.id]
	if route == nil || route.identity != token.identity {
		return nil, ErrClosed
	}
	return route, h.validateRouteUsesLocked(route)
}

func (h *Host) ValidateEventRoute(ctx context.Context, route *EventRoute) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.liveRouteLocked(route)
	return err
}

// DeliverEvent registers delivery in both owners before calling application
// code. A callback may reenter Host; no Host or admission lock crosses it.
func DeliverEvent[T any](ctx context.Context, host *Host, route *EventRoute, event Event[T], payload T, receive func(context.Context, T) error) error {
	if host == nil || route == nil || event.value == nil || receive == nil || route.Source().Contract != event.Ref() {
		return ErrContractMismatch
	}
	sourceWork, receiverWork, err := host.enterRoute(ctx, route, event.value)
	if err != nil {
		return err
	}
	defer sourceWork.Finish()
	defer receiverWork.Finish()
	delivery, cancel := context.WithCancelCause(sourceWork.Context())
	defer cancel(ErrClosed)
	stop := context.AfterFunc(receiverWork.Context(), func() { cancel(context.Cause(receiverWork.Context())) })
	defer stop()
	if err := contextError(delivery); err != nil {
		return err
	}
	return errors.Join(receive(delivery, payload), contextError(delivery))
}

func (h *Host) enterRoute(ctx context.Context, token *EventRoute, event *declaration) (*Work, *Work, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.contracts.matches(event) {
		return nil, nil, ErrContractMismatch
	}
	route, err := h.liveRouteLocked(token)
	if err != nil {
		return nil, nil, err
	}
	sourceWork, err := route.source.lifetime.Enter(ctx)
	if err != nil {
		return nil, nil, err
	}
	receiverWork, err := route.receiver.lifetime.Enter(ctx)
	if err != nil {
		sourceWork.Finish()
		return nil, nil, err
	}
	return sourceWork, receiverWork, nil
}

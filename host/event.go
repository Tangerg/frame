package host

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
)

// EventRoute is an opaque authorization to communicate between two exact
// objects in already admitted Uses. It contains no handler or native payload.
type EventRoute struct{ identity *eventRouteIdentity }

type eventRouteIdentity struct {
	id        string
	authority *frame.LifetimeDomain
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
	identity  *eventRouteIdentity
	source    *useBorrow
	receiver  *useBorrow
	resources []*frame.ResourceHandle
}

const eventRoutePrefix = "event route:"

func (r *eventRoute) participants() []*useBorrow {
	if r.source == r.receiver {
		return []*useBorrow{r.source}
	}
	return []*useBorrow{r.source, r.receiver}
}

func (r *eventRoute) releaseResourcesLocked() {
	for _, resource := range r.resources {
		_ = resource.ConfirmReleased(nil)
	}
	r.resources = nil
}

func (u *useBorrow) exports(source ExportRef) bool {
	for _, member := range u.members {
		if member.source == source {
			return true
		}
	}
	return false
}

func (h *hostState) NewEventRoute(ctx context.Context, sourceUse, receiverUse *Use, source, receiver ExportRef) (*EventRoute, error) {
	if sourceUse == nil || receiverUse == nil || sourceUse.borrow == nil || receiverUse.borrow == nil || sourceUse.borrow.authority != h.identity || receiverUse.borrow.authority != h.identity {
		return nil, frame.ErrInvalid
	}
	if source.Contract.Kind != EventKind || source.Contract != receiver.Contract {
		return nil, frame.ErrContractMismatch
	}
	route := &eventRoute{identity: &eventRouteIdentity{id: rand.Text(), authority: h.identity, source: source, receiver: receiver}, source: sourceUse.borrow, receiver: receiverUse.borrow}
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

func (h *hostState) admitRouteLocked(route *eventRoute) error {
	if err := h.validateRouteUsesLocked(route); err != nil {
		return err
	}
	if !route.source.exports(route.identity.source) || !route.receiver.exports(route.identity.receiver) {
		return frame.ErrContractMismatch
	}
	if route.source.realm != route.receiver.realm && route.identity.source.Contract.Mode == NativeMode {
		return frame.ErrContractMismatch
	}
	if len(h.routes) >= h.limits.MaxEventRoutes {
		return fmt.Errorf("%w: event routes", frame.ErrCapacity)
	}
	return nil
}

func (h *hostState) ownRouteLocked(route *eventRoute) error {
	resource := frame.Resource{Name: eventRoutePrefix + route.identity.id, Close: func(context.Context) (frame.ReleaseState, error) {
		h.releaseEventRoute(route)
		return frame.ResourceReleased, nil
	}}
	for _, participant := range route.participants() {
		owned, err := participant.lifetime.Own(resource)
		if err != nil {
			route.releaseResourcesLocked()
			return err
		}
		route.resources = append(route.resources, owned)
	}
	h.routes[route.identity.id] = route
	return nil
}

func (h *hostState) releaseEventRoute(route *eventRoute) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.routes[route.identity.id] == route {
		delete(h.routes, route.identity.id)
	}
	route.releaseResourcesLocked()
}

func (h *hostState) validateRouteUsesLocked(route *eventRoute) error {
	for _, participant := range route.participants() {
		if err := h.validateUseLocked(participant); err != nil {
			return err
		}
	}
	return nil
}

func (h *hostState) liveRouteLocked(token *EventRoute) (*eventRoute, error) {
	if token == nil || token.identity == nil || token.identity.authority != h.identity {
		return nil, frame.ErrInvalid
	}
	route := h.routes[token.identity.id]
	if route == nil || route.identity != token.identity {
		return nil, frame.ErrClosed
	}
	return route, h.validateRouteUsesLocked(route)
}

func (h *hostState) ValidateEventRoute(ctx context.Context, route *EventRoute) error {
	if err := frame.ContextError(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.liveRouteLocked(route)
	return err
}

// DeliverEvent registers delivery in each owner before calling application
// code. A callback may reenter Host; no Host or admission lock crosses it.
func DeliverEvent[T any](ctx context.Context, host Host, route *EventRoute, event Event[T], payload T, receive func(context.Context, T) error) error {
	if !host.Valid() || route == nil || event.value == nil || receive == nil || route.Source().Contract != event.Ref() {
		return frame.ErrContractMismatch
	}
	delivery, err := host.enterRoute(ctx, route, event.value)
	if err != nil {
		return err
	}
	defer delivery.finish()
	if err := delivery.check(); err != nil {
		return err
	}
	return errors.Join(receive(delivery.ctx, payload), delivery.check())
}

// Notifications cancel the composed context. Original Uses and intervals
// conclude admission, so a queued notification cannot authorize completion.
type eventDelivery struct {
	participations []eventParticipation
	ctx            context.Context
	cancel         context.CancelCauseFunc
	stops          []func() bool
}

type eventParticipation struct {
	use      *useBorrow
	interval *frame.Work
}

func (d *eventDelivery) check() error {
	for _, participation := range d.participations {
		if err := frame.ContextError(participation.interval.Context()); err != nil {
			d.cancel(context.Cause(participation.interval.Context()))
			return err
		}
		if err := participation.use.validate(); err != nil {
			d.cancel(err)
			return err
		}
	}
	return nil
}

func (d *eventDelivery) finish() {
	for _, stop := range d.stops {
		stop()
	}
	if d.cancel != nil {
		d.cancel(frame.ErrClosed)
	}
	for _, participation := range d.participations {
		participation.interval.Finish()
	}
}

func (h *hostState) enterRoute(ctx context.Context, token *EventRoute, event *declaration) (*eventDelivery, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.contracts.matches(event) {
		return nil, frame.ErrContractMismatch
	}
	route, err := h.liveRouteLocked(token)
	if err != nil {
		return nil, err
	}
	delivery := &eventDelivery{}
	for _, participant := range route.participants() {
		interval, err := participant.enter(ctx)
		if err != nil {
			delivery.finish()
			return nil, err
		}
		delivery.participations = append(delivery.participations, eventParticipation{use: participant, interval: interval})
	}
	delivery.ctx, delivery.cancel = context.WithCancelCause(delivery.participations[0].interval.Context())
	for _, participation := range delivery.participations[1:] {
		interval := participation.interval
		delivery.stops = append(delivery.stops, context.AfterFunc(interval.Context(), func() {
			delivery.cancel(context.Cause(interval.Context()))
		}))
	}
	return delivery, nil
}

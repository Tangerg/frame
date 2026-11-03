package frame

import (
	"context"
	"errors"
)

// EventRoute is an opaque authorization to communicate between two exact
// objects in already admitted Uses. It contains no handler or native payload.
type EventRoute struct {
	id            string
	owner         *authorityIdentity
	source        ExportRef
	receiver      ExportRef
	sourceUseID   string
	receiverUseID string
}

func (r *EventRoute) ID() string            { return r.id }
func (r *EventRoute) Source() ExportRef     { return r.source }
func (r *EventRoute) Receiver() ExportRef   { return r.receiver }
func (r *EventRoute) SourceUseID() string   { return r.sourceUseID }
func (r *EventRoute) ReceiverUseID() string { return r.receiverUseID }

type eventRoute struct {
	token            *EventRoute
	source           *Use
	receiver         *Use
	sourceResource   *ResourceHandle
	receiverResource *ResourceHandle
}

func ownsExport(use *Use, source ExportRef) bool {
	for _, member := range use.members {
		if member.source == source {
			return true
		}
	}
	return false
}

func (h *Host) NewEventRoute(ctx context.Context, sourceUse, receiverUse *Use, source, receiver ExportRef) (*EventRoute, error) {
	if sourceUse == nil || receiverUse == nil || sourceUse.owner != h.identity || receiverUse.owner != h.identity {
		return nil, ErrInvalid
	}
	if source.Contract.Kind != EventKind || source.Contract != receiver.Contract {
		return nil, ErrContractMismatch
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	token := &EventRoute{id: id, owner: h.identity, source: source, receiver: receiver, sourceUseID: sourceUse.id, receiverUseID: receiverUse.id}
	record := &eventRoute{token: token, source: sourceUse, receiver: receiverUse}
	err = h.admission.Admit(ctx, func(context.Context) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.isClosing() || h.uses[sourceUse.id] != sourceUse || h.uses[receiverUse.id] != receiverUse {
			return ErrClosed
		}
		if err := h.admission.ValidateScope(sourceUse.scope); err != nil {
			return err
		}
		if err := h.admission.ValidateScope(receiverUse.scope); err != nil {
			return err
		}
		if !ownsExport(sourceUse, source) || !ownsExport(receiverUse, receiver) {
			return ErrContractMismatch
		}
		if sourceUse.realm != receiverUse.realm && source.Contract.Mode == NativeMode {
			return ErrContractMismatch
		}
		if len(h.routes) >= h.maxEventRoutes {
			return ErrCapacity
		}
		resource := Resource{Name: "event route:" + id, Close: func(context.Context) (ReleaseState, error) { h.releaseEventRoute(record); return ResourceReleased, nil }}
		sourceResource, err := sourceUse.lifetime.Own(resource)
		if err != nil {
			return err
		}
		record.sourceResource = sourceResource
		if sourceUse != receiverUse {
			receiverResource, err := receiverUse.lifetime.Own(resource)
			if err != nil {
				_ = sourceResource.ConfirmReleased(nil)
				return err
			}
			record.receiverResource = receiverResource
		}
		h.routes[id] = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

func (h *Host) releaseEventRoute(record *eventRoute) {
	h.mu.Lock()
	if h.routes[record.token.id] == record {
		delete(h.routes, record.token.id)
	}
	source := record.sourceResource
	receiver := record.receiverResource
	record.source = nil
	record.receiver = nil
	h.mu.Unlock()
	if source != nil {
		_ = source.ConfirmReleased(nil)
	}
	if receiver != nil {
		_ = receiver.ConfirmReleased(nil)
	}
}

func (h *Host) eventRouteLocked(route *EventRoute) (*eventRoute, error) {
	if route == nil || route.owner != h.identity {
		return nil, ErrInvalid
	}
	record := h.routes[route.id]
	if record == nil || record.token != route || record.source == nil || record.receiver == nil {
		return nil, ErrClosed
	}
	if h.lifetime.isClosing() || h.uses[route.sourceUseID] != record.source || h.uses[route.receiverUseID] != record.receiver {
		return nil, ErrClosed
	}
	return record, nil
}

func (h *Host) ValidateEventRoute(ctx context.Context, route *EventRoute) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	record, err := h.eventRouteLocked(route)
	if err != nil {
		return err
	}
	if err := h.admission.ValidateScope(record.source.scope); err != nil {
		return err
	}
	return h.admission.ValidateScope(record.receiver.scope)
}

// DeliverEvent registers delivery in both owners before calling application
// code. A callback may reenter Host; no Host or admission lock crosses it.
func DeliverEvent[T any](ctx context.Context, host *Host, route *EventRoute, event Event[T], payload T, receive func(context.Context, T) error) error {
	if host == nil || route == nil || event.value == nil || receive == nil || route.source.Contract != event.Ref() {
		return ErrContractMismatch
	}
	host.mu.Lock()
	if !sameDeclaration(host.contracts[event.Ref()], event.value) {
		host.mu.Unlock()
		return ErrContractMismatch
	}
	record, err := host.eventRouteLocked(route)
	if err != nil {
		host.mu.Unlock()
		return err
	}
	if err := host.admission.ValidateScope(record.source.scope); err != nil {
		host.mu.Unlock()
		return err
	}
	if err := host.admission.ValidateScope(record.receiver.scope); err != nil {
		host.mu.Unlock()
		return err
	}
	sourceWork, err := record.source.lifetime.Enter(ctx)
	if err != nil {
		host.mu.Unlock()
		return err
	}
	receiverWork, err := record.receiver.lifetime.Enter(ctx)
	host.mu.Unlock()
	if err != nil {
		sourceWork.Finish()
		return err
	}
	defer sourceWork.Finish()
	defer receiverWork.Finish()
	deliveryCtx, cancel := context.WithCancelCause(sourceWork.Context())
	stop := context.AfterFunc(receiverWork.Context(), func() { cancel(context.Cause(receiverWork.Context())) })
	defer stop()
	defer cancel(ErrClosed)
	if err := contextError(deliveryCtx); err != nil {
		return err
	}
	return errors.Join(receive(deliveryCtx, payload), contextError(deliveryCtx))
}

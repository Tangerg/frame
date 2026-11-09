package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Tangerg/frame/internal/cancellation"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

// CallbackReceiverLimits bounds simultaneously live grants, complete JSON
// request/reply bodies, and the time an incoming request can occupy a socket.
type CallbackReceiverLimits struct {
	MaxCalls         int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	ReadTimeout      time.Duration
}

// CallbackReceiver owns one loopback listener per live grant and none when idle.
type CallbackReceiver struct {
	limits CallbackReceiverLimits
	calls  chan struct{}
}

func NewCallbackReceiver(limits CallbackReceiverLimits) (*CallbackReceiver, error) {
	if limits.MaxCalls < 1 || limits.ReadTimeout <= 0 ||
		limits.MaxRequestBytes < 1 || limits.MaxRequestBytes >= math.MaxInt ||
		limits.MaxResponseBytes < 1 || limits.MaxResponseBytes >= math.MaxInt {
		return nil, frame.ErrInvalid
	}
	return &CallbackReceiver{limits, make(chan struct{}, limits.MaxCalls)}, nil
}

type callbackRoute struct {
	name, fingerprint string
	kind              portable.MethodKind
	handle            func(context.Context, string, value.JSON) (value.JSON, error)
}

// Grant binds one unary method to ctx. The host owns authorization, effects,
// and receipts. The handler receives the original command ID (empty for queries).
func (r *CallbackReceiver) Grant(ctx context.Context, method *portable.Method, handle func(context.Context, string, value.JSON) (value.JSON, error)) (ManagedCallback, error) {
	if r == nil || method == nil || handle == nil {
		return ManagedCallback{}, frame.ErrInvalid
	}
	method = new(*method)
	if method.Identity().Validate() != nil || len(method.Descriptor().Stream) != 0 {
		return ManagedCallback{}, frame.ErrInvalid
	}
	return r.open(ctx, callbackRoute{method.Name(), method.Identity().Digest().String(), method.Kind(), func(call context.Context, id string, input value.JSON) (value.JSON, error) {
		if err := method.ValidateInput(input); err != nil {
			return value.JSON{}, err
		}
		output, err := handle(call, id, input)
		if err == nil {
			if int64(len(output.String())) > r.limits.MaxResponseBytes {
				return value.JSON{}, frame.ErrCapacity
			}
			err = method.ValidateOutput(output.Bytes())
		}
		return output, err
	}})
}

// The independent portable/1 vector pins the qualified delivery DTO schema.
const streamDeliveryName = "qimu.stream/deliver.v1"
const streamDeliveryFingerprint = "4d188c8a5e895f61b9360da82f3e9f9bdb89f1b3f0af516336ab90cb5b0134d6"

// Stream implements OpenManagedStream. A positive ACK follows consumer
// acceptance. The managed invocation owns ordering and chunk validation.
func (r *CallbackReceiver) Stream(ctx context.Context, session *sdk.ClientSession, receive func(context.Context, uint64, value.JSON) error) (ManagedCallback, error) {
	if r == nil || session == nil || receive == nil {
		return ManagedCallback{}, frame.ErrInvalid
	}
	ack, err := value.EncodeJSON(true)
	if err != nil {
		return ManagedCallback{}, err
	}
	envelope, err := json.Marshal(managedResult{Value: ack.Bytes()})
	if err != nil {
		return ManagedCallback{}, err
	}
	if int64(len(envelope)) > r.limits.MaxResponseBytes {
		return ManagedCallback{}, frame.ErrCapacity
	}
	return r.open(ctx, callbackRoute{streamDeliveryName, streamDeliveryFingerprint, portable.Command, func(call context.Context, _ string, input value.JSON) (value.JSON, error) {
		var part struct {
			Sequence uint64     `json:"sequence"`
			Value    value.JSON `json:"value"`
		}
		if err := value.DecodeValue(input.Bytes(), &part, "stream delivery"); err != nil {
			return value.JSON{}, err
		}
		if part.Sequence == 0 || part.Value.Validate() != nil {
			return value.JSON{}, frame.ErrInvalid
		}
		if err := receive(call, part.Sequence, part.Value); err != nil {
			return value.JSON{}, err
		}
		return ack, nil
	}})
}

func credentialMatches(header http.Header, name, expected string) bool {
	tokens := header.Values(name)
	return len(tokens) == 1 && subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(expected)) == 1
}

func (route callbackRoute) admits(p *managedParams) bool {
	return p.Protocol == managedVersion && p.Method == route.name && p.Contract.String() == route.fingerprint &&
		p.Contributions == nil && p.Export == "" && p.Capability.String() == "" && p.Meta == nil &&
		(route.kind == portable.Command && portable.ValidCommandID(p.CommandID) || route.kind == portable.Query && p.CommandID == "")
}

// A callback request presents both the session credential and the call
// identity of the original grant.
const (
	// A callback listens only on loopback: the receiver trusts the local user
	// that runs the plugin, never the network.
	loopbackHost   = "127.0.0.1"
	callbackScheme = "http"

	credentialHeader = "Authorization"
	callHeader       = "Qimu-Call"
)

func (route callbackRoute) serve(w http.ResponseWriter, req *http.Request, ref ManagedCallback, limits CallbackReceiverLimits) {
	params, input, status := route.read(w, req, ref, limits)
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	output, err := route.handle(req.Context(), params.CommandID, input)
	data, ok := encodeCallbackReply(output, err, limits)
	if !ok || req.Context().Err() != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	// Start the socket write budget after the host's application work has finished.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(limits.ReadTimeout)); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (route callbackRoute) read(w http.ResponseWriter, req *http.Request, ref ManagedCallback, limits CallbackReceiverLimits) (managedParams, value.JSON, int) {
	if req.Method != http.MethodPost || req.RequestURI != "/" ||
		!credentialMatches(req.Header, credentialHeader, ref.Credential) || !credentialMatches(req.Header, callHeader, ref.Call) {
		return managedParams{}, value.JSON{}, http.StatusForbidden
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, limits.MaxRequestBytes))
	if err != nil {
		return managedParams{}, value.JSON{}, http.StatusRequestEntityTooLarge
	}
	var params managedParams
	if err = json.Unmarshal(body, &params, json.RejectUnknownMembers(true)); err != nil || !route.admits(&params) {
		return managedParams{}, value.JSON{}, http.StatusBadRequest
	}
	input, err := value.ParseJSON(params.Input)
	if err != nil || req.Context().Err() != nil {
		return managedParams{}, value.JSON{}, http.StatusBadRequest
	}
	return params, input, http.StatusOK
}

// encodeCallbackReply admits a result or a typed Refusal within the response
// budget; any other failure cannot be reported to the plugin.
func encodeCallbackReply(output value.JSON, failure error, limits CallbackReceiverLimits) ([]byte, bool) {
	var reply managedResult
	if failure == nil {
		if int64(len(output.String())) > limits.MaxResponseBytes {
			return nil, false
		}
		reply.Value = output.Bytes()
	} else {
		if _, interrupted := errors.AsType[*callbackPanic](failure); interrupted {
			return nil, false
		}
		refusal, known := errors.AsType[*Refusal](failure)
		if !known || refusal == nil || refusal.Code == "" || int64(len(refusal.Code))+int64(len(refusal.Message)) > limits.MaxResponseBytes {
			return nil, false
		}
		encoded, err := json.Marshal(refusal)
		if err != nil {
			return nil, false
		}
		reply.Failure = encoded
	}
	data, err := json.Marshal(&reply)
	if err != nil || int64(len(data)) > limits.MaxResponseBytes {
		return nil, false
	}
	return data, true
}

// Limit admitted sockets before net/http can allocate connection goroutines.
type callbackListener struct {
	net.Listener
	ctx    context.Context
	active chan struct{}
}

func (l callbackListener) Accept() (net.Conn, error) {
	select {
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	case l.active <- struct{}{}:
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.active
	}
	return conn, err
}

func (r *CallbackReceiver) open(ctx context.Context, route callbackRoute) (ManagedCallback, error) {
	if r == nil || r.calls == nil || ctx == nil {
		return ManagedCallback{}, frame.ErrInvalid
	}
	if err := cancellation.Err(ctx); err != nil {
		return ManagedCallback{}, err
	}
	select {
	case r.calls <- struct{}{}:
	default:
		return ManagedCallback{}, frame.ErrCapacity
	}
	conn, err := new(net.ListenConfig).Listen(ctx, "tcp4", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		<-r.calls
		return ManagedCallback{}, err
	}
	var tokens [2 * managedCallbackTokenBytes]byte
	rand.Read(tokens[:])
	ref := ManagedCallback{
		Endpoint: callbackScheme + "://" + conn.Addr().String() + "/",
		Call:     hex.EncodeToString(tokens[:managedCallbackTokenBytes]), Credential: hex.EncodeToString(tokens[managedCallbackTokenBytes:]),
		MaxResponseBytes: r.limits.MaxResponseBytes,
	}
	life, cancel := context.WithCancel(ctx)
	l := callbackListener{conn, life, make(chan struct{}, 1)}
	// The socket slot serializes handlers and joins them before grant release,
	// so the original grant retains its first interruption without another owner.
	var callbackErr error
	server := &http.Server{
		ReadTimeout:    r.limits.ReadTimeout,
		MaxHeaderBytes: int(min(r.limits.MaxRequestBytes, int64(http.DefaultMaxHeaderBytes))),
		BaseContext:    func(net.Listener) context.Context { return life },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			returned := false
			defer func() {
				var interrupted error
				if recovered := recover(); recovered != nil {
					interrupted = newCallbackPanic("mcp: callback handler panicked", recovered)
				} else if !returned {
					interrupted = errors.New("mcp: callback handler exited without returning")
				}
				if interrupted != nil {
					if callbackErr == nil {
						callbackErr = interrupted
					}
					w.WriteHeader(http.StatusUnprocessableEntity)
				}
			}()
			route.serve(w, req, ref, r.limits)
			returned = true
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				<-l.active
			}
		},
	}
	// An idle HTTP client cannot monopolize this single-call endpoint.
	server.SetKeepAlivesEnabled(false)
	served := make(chan struct{})
	var serveErr error
	go func() {
		defer close(served)
		if err := server.Serve(l); !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			serveErr = err
		}
	}()
	stop, released := make(chan struct{}), make(chan struct{})
	closeStop := sync.OnceFunc(func() { close(stop) })
	var closeErr error
	go func() {
		defer close(released)
		select {
		case <-life.Done():
		case <-stop:
		case <-served:
		}
		cancel()
		err := server.Close()
		<-served
		// Serve can return while a callback is draining. StateClosed releases the
		// slot only after the last admitted handler has actually returned.
		l.active <- struct{}{}
		closeErr = errors.Join(callbackErr, serveErr, err)
		<-r.calls
	}()
	grant := ref
	grant.Released = released
	grant.Revoke = func() error { closeStop(); <-released; return closeErr }
	return grant, nil
}

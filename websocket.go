package phoenix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	gqlgenTransport "github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gorilla/websocket"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const phoenixSubprotocol = "phoenix"

var (
	_ graphql.Transport = Websocket{}
)

type (
	// Websocket is a custom websocket transport that extends the default
	// transport with Phoenix-specific functionality.  Phoenix is a web framework
	// for Elixir that uses websockets for real-time communication; the goal of this
	// transport is to provide a way for the GraphQL server to communicate with
	// Phoenix clients.
	Websocket struct {
		Upgrader    websocket.Upgrader
		InitFunc    WebsocketInitFunc
		InitTimeout time.Duration
		ErrorFunc   WebsocketErrorFunc
		CloseFunc   WebsocketCloseFunc
	}
	// InitPayload is a structure that is parsed from the websocket init message payload. TO use
	// request headers for non-websocket, instead wrap the graphql handler in a middleware.
	InitPayload map[string]interface{}

	wsConnection struct {
		Websocket
		ctx    context.Context
		conn   *websocket.Conn
		me     messageExchanger
		active map[string]context.CancelFunc
		mu     sync.Mutex
		exec   graphql.GraphExecutor
		closed bool

		initPayload InitPayload
	}

	WebsocketInitFunc  func(ctx context.Context, initPayload InitPayload) (context.Context, *InitPayload, error)
	WebsocketErrorFunc func(ctx context.Context, err error)

	// Callback called when websocket is closed.
	WebsocketCloseFunc func(ctx context.Context, closeCode int)

	subscriptionPayload struct {
		SubscriptionID string `json:"subscriptionId"`
	}
)

var errReadTimeout = errors.New("read timeout")

func (t Websocket) Supports(r *http.Request) bool {
	return r.Header.Get("Upgrade") != ""
}

func (t Websocket) Do(w http.ResponseWriter, r *http.Request, exec graphql.GraphExecutor) {
	ws, err := t.Upgrader.Upgrade(w, r, http.Header{})
	if err != nil {
		log.Printf("unable to upgrade %T to websocket %s: ", w, err.Error())
		gqlgenTransport.SendErrorf(w, http.StatusBadRequest, "unable to upgrade")
		return
	}

	var me messageExchanger
	switch ws.Subprotocol() {
	default:
		msg := websocket.FormatCloseMessage(websocket.CloseProtocolError, fmt.Sprintf("unsupported negotiated subprotocol %s", ws.Subprotocol()))
		_ = ws.WriteMessage(websocket.CloseMessage, msg)
		return
	case phoenixSubprotocol, "":
		// clients are supposed to send a subprotocol, however in every real-world example I've seen, the subprotocol header is missing
		// so let's assume "phoenix" by default
		me = MessageExchanger{c: ws}
	}

	conn := wsConnection{
		active:    map[string]context.CancelFunc{},
		conn:      ws,
		ctx:       r.Context(),
		exec:      exec,
		me:        me,
		Websocket: t,
	}

	if !conn.init() {
		return
	}

	conn.run()
}

func (c *wsConnection) handlePossibleError(err error, isReadError bool) {
	if c.ErrorFunc != nil && err != nil {
		c.ErrorFunc(c.ctx, gqlgenTransport.WebsocketError{
			Err:         err,
			IsReadError: isReadError,
		})
	}
}

func (c *wsConnection) nextMessageWithTimeout(timeout time.Duration) (Message, error) {
	messages, errs := make(chan Message, 1), make(chan error, 1)

	go func() {
		if m, err := c.me.NextMessage(); err != nil {
			errs <- err
		} else {
			messages <- m
		}
	}()

	select {
	case m := <-messages:
		return m, nil
	case err := <-errs:
		return Message{}, err
	case <-time.After(timeout):
		return Message{}, errReadTimeout
	}
}

func (c *wsConnection) init() bool {
	var m Message
	var err error

	if c.InitTimeout != 0 {
		m, err = c.nextMessageWithTimeout(c.InitTimeout)
	} else {
		m, err = c.me.NextMessage()
	}

	if err != nil {
		if err == errReadTimeout {
			c.close(websocket.CloseProtocolError, "connection initialisation timeout")
			return false
		}

		if err == errInvalidMsg {
			c.sendConnectionError("invalid json")
		}

		c.close(websocket.CloseProtocolError, "decoding error")
		return false
	}

	switch m.Type() {
	case phxJoinMessageType:
		if len(m.Payload) > 0 {
			c.initPayload = make(InitPayload)
			err := json.Unmarshal(m.Payload, &c.initPayload)
			if err != nil {
				return false
			}
		}

		var initAckPayload *InitPayload = nil
		if c.InitFunc != nil {
			var ctx context.Context
			ctx, initAckPayload, err = c.InitFunc(c.ctx, c.initPayload)
			if err != nil {
				c.sendConnectionError("%s", err.Error())
				c.close(websocket.CloseNormalClosure, "terminated")
				return false
			}
			c.ctx = ctx
		}

		if initAckPayload != nil {
			initJsonAckPayload, err := json.Marshal(*initAckPayload)
			if err != nil {
				panic(err)
			}
			c.write(&Message{
				JoinRef: m.JoinRef,
				Ref:     m.Ref,
				Topic:   m.Topic,
				Event:   phxReplyMessageType.String(),
				Payload: initJsonAckPayload})
		} else {
			c.write(&Message{
				JoinRef: m.JoinRef,
				Ref:     m.Ref,
				Topic:   m.Topic,
				Event:   phxReplyMessageType.String()})
		}
	case phxCloseMessageType:
		c.close(websocket.CloseNormalClosure, "terminated")
		return false
	default:
		c.sendConnectionError("unexpected message %s", m.Event)
		c.close(websocket.CloseProtocolError, "unexpected message")
		return false
	}

	return true
}

func (c *wsConnection) write(msg *Message) {
	c.mu.Lock()
	c.handlePossibleError(c.me.Send(msg), false)
	c.mu.Unlock()
}

func (c *wsConnection) run() {
	// We create a cancellation that will shutdown the keep-alive when we leave
	// this function.
	ctx, cancel := context.WithCancel(c.ctx)
	defer func() {
		cancel()
		c.close(websocket.CloseAbnormalClosure, "unexpected closure")
	}()

	// Close the connection when the context is cancelled.
	// Will optionally send a "close reason" that is retrieved from the context.
	go c.closeOnCancel(ctx)

	for {
		start := graphql.Now()
		m, err := c.me.NextMessage()
		if err != nil {
			// If the connection got closed by us, don't report the error
			if !errors.Is(err, net.ErrClosed) {
				c.handlePossibleError(err, true)
			}
			return
		}

		switch m.Type() {
		// "doc" is used for gql subscriptions
		case docMessageType:
			// Ensure we have non-nil values for the fields we care about
			if m.Ref == nil || m.JoinRef == nil || m.Topic == nil {
				c.sendConnectionError("missing required fields")
				c.close(websocket.CloseProtocolError, "missing required fields")
				return
			}
			c.subscribe(m.Ref, m.JoinRef, m.Topic, start, &m)
		// "unsubscribe" is used to cancel a subscription
		case unsubscribeMessageType:
			pl := subscriptionPayload{}
			if err := json.Unmarshal(m.Payload, &pl); err != nil {
				c.sendConnectionError("invalid json")
				c.close(websocket.CloseProtocolError, "invalid json")
				return
			}
			c.mu.Lock()
			closer, ok := c.active[pl.SubscriptionID]
			c.mu.Unlock()
			if !ok {
				c.sendConnectionError("unknown subscription ID")
				return
			}
			if closer != nil {
				closer()
			}
			// send an ack with terminated subscription ID
			c.send(m.Ref, m.JoinRef, m.Topic, phxReplyMessageType.String(), &subscriptionPayload{SubscriptionID: pl.SubscriptionID})
		// "phx_close" is used to close the connection
		case phxCloseMessageType:
			c.close(websocket.CloseNormalClosure, "terminated")
			return
		// "heartbeat" is used to keep the connection alive
		case heartbeatMessageType:
			c.write(&Message{
				Ref:     m.Ref,
				JoinRef: m.JoinRef,
				Topic:   m.Topic,
				Event:   heartbeatMessageType.String()})
		// "phx_leave" is used to leave a topic
		case phxLeaveMessageType:
			// TODO: Implement leave

		// TODO: handle the rest of the message types
		default:
			c.sendConnectionError("unexpected message %s", m.Event)
			c.close(websocket.CloseProtocolError, "unexpected message")
			return
		}
	}
}

func (c *wsConnection) closeOnCancel(ctx context.Context) {
	<-ctx.Done()

	if r := closeReasonForContext(ctx); r != "" {
		c.sendConnectionError("%s", r)
	}
	c.close(websocket.CloseNormalClosure, "terminated")
}

func (c *wsConnection) subscribe(id, joinRef, topic *string, start time.Time, msg *Message) {
	ctx := graphql.StartOperationTrace(c.ctx)
	var params *graphql.RawParams
	if err := jsonDecode(bytes.NewReader(msg.Payload), &params); err != nil {
		c.sendError(id, joinRef, topic, &gqlerror.Error{Message: "invalid json"})
		return
	}

	params.ReadTime = graphql.TraceTiming{
		Start: start,
		End:   graphql.Now(),
	}

	rc, err := c.exec.CreateOperationContext(ctx, params)
	if err != nil {
		resp := c.exec.DispatchError(graphql.WithOperationContext(ctx, rc), err)
		switch errcode.GetErrorKind(err) {
		case errcode.KindProtocol:
			c.sendError(msg.Ref, msg.JoinRef, msg.Topic, resp.Errors...)
		default:
			c.sendResponse(msg.Ref, msg.JoinRef, msg.Topic, phxReplyMessageType.String(), &graphql.Response{Errors: err})
		}

		return
	}

	ctx = graphql.WithOperationContext(ctx, rc)

	// TODO: Fix this
	// if c.initPayload != nil {
	// 	ctx = withInitPayload(ctx, c.initPayload)
	// }

	// Generate a subscription ID and use that as the ref
	subscriptionID, err2 := generateSubscriptionID()
	if err2 != nil {
		c.sendError(id, joinRef, topic, &gqlerror.Error{Message: "failed to generate subscription ID"})
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.active[subscriptionID] = cancel
	c.mu.Unlock()

	go func() {
		ctx = withSubscriptionErrorContext(ctx)
		defer func() {
			if r := recover(); r != nil {
				err := rc.Recover(ctx, r)
				var gqlerr *gqlerror.Error
				if !errors.As(err, &gqlerr) {
					gqlerr = &gqlerror.Error{}
					if err != nil {
						gqlerr.Message = err.Error()
					}
				}
				c.sendError(msg.Ref, msg.JoinRef, msg.Topic, gqlerr)
			}
			if errs := getSubscriptionError(ctx); len(errs) != 0 {
				c.sendError(msg.Ref, msg.JoinRef, msg.Topic, errs...)
			}
			c.mu.Lock()
			delete(c.active, subscriptionID)
			c.mu.Unlock()
			cancel()
		}()

		// Send subscription ack
		c.send(msg.Ref, msg.JoinRef, msg.Topic, phxReplyMessageType.String(), &subscriptionPayload{SubscriptionID: subscriptionID})

		responses, ctx := c.exec.DispatchOperation(ctx, rc)
		for {
			response := responses(ctx)
			if response == nil {
				break
			}

			c.sendResponse(nil, msg.JoinRef, &subscriptionID, subscriptionData.String(), response)
		}

		// complete and context cancel comes from the defer
	}()
}

func (c *wsConnection) send(id, joinRef, topic *string, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	c.write(&Message{
		Ref:     id,
		JoinRef: joinRef,
		Topic:   topic,
		Event:   event,
		Payload: b,
	})
}

func (c *wsConnection) sendResponse(id, joinRef, topic *string, event string, response *graphql.Response) {
	c.send(id, joinRef, topic, event, response)
}

func (c *wsConnection) sendError(id, joinRef, topic *string, errors ...*gqlerror.Error) {
	errs := make([]error, len(errors))
	for i, err := range errors {
		errs[i] = err
	}
	b, err := json.Marshal(errs)
	if err != nil {
		panic(err)
	}
	c.write(&Message{
		Ref:     id,
		JoinRef: joinRef,
		Topic:   topic,
		Event:   phxErrorMessageType.String(),
		Payload: b})
}

func (c *wsConnection) sendConnectionError(format string, args ...interface{}) {
	b, err := json.Marshal(&gqlerror.Error{Message: fmt.Sprintf(format, args...)})
	if err != nil {
		panic(err)
	}

	c.write(&Message{
		Event:   phxErrorMessageType.String(),
		Payload: b})
}

func (c *wsConnection) close(closeCode int, message string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(closeCode, message))
	for _, closer := range c.active {
		closer()
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close()

	if c.CloseFunc != nil {
		c.CloseFunc(c.ctx, closeCode)
	}
}

// A private key for context that only this package can access. This is important
// to prevent collisions between different context uses
var closeReasonCtxKey = &wsCloseReasonContextKey{"close-reason"}

type wsCloseReasonContextKey struct {
	name string
}

func AppendCloseReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, closeReasonCtxKey, reason)
}

func closeReasonForContext(ctx context.Context) string {
	reason, _ := ctx.Value(closeReasonCtxKey).(string)
	return reason
}

// A private key for context that only this package can access. This is important
// to prevent collisions between different context uses
var wsSubscriptionErrorCtxKey = &wsSubscriptionErrorContextKey{"subscription-error"}

type wsSubscriptionErrorContextKey struct {
	name string
}

type subscriptionError struct {
	errs []*gqlerror.Error
}

// AddSubscriptionError is used to let websocket return an error message after subscription resolver returns a channel.
// for example:
//
//	func (r *subscriptionResolver) Method(ctx context.Context) (<-chan *model.Message, error) {
//		ch := make(chan *model.Message)
//		go func() {
//	     defer func() {
//				close(ch)
//	     }
//			// some kind of block processing (e.g.: gRPC client streaming)
//			stream, err := gRPCClientStreamRequest(ctx)
//			if err != nil {
//				   transport.AddSubscriptionError(ctx, err)
//	            return // must return and close channel so websocket can send error back
//	     }
//			for {
//				m, err := stream.Recv()
//				if err == io.EOF {
//					return
//				}
//				if err != nil {
//				   transport.AddSubscriptionError(ctx, err)
//	            return // must return and close channel so websocket can send error back
//				}
//				ch <- m
//			}
//		}()
//
//		return ch, nil
//	}
//
// see https://github.com/99designs/gqlgen/pull/2506 for more details
func AddSubscriptionError(ctx context.Context, err *gqlerror.Error) {
	subscriptionErrStruct := getSubscriptionErrorStruct(ctx)
	subscriptionErrStruct.errs = append(subscriptionErrStruct.errs, err)
}

func withSubscriptionErrorContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, wsSubscriptionErrorCtxKey, &subscriptionError{})
}

func getSubscriptionErrorStruct(ctx context.Context) *subscriptionError {
	v, _ := ctx.Value(wsSubscriptionErrorCtxKey).(*subscriptionError)
	return v
}

func getSubscriptionError(ctx context.Context) []*gqlerror.Error {
	return getSubscriptionErrorStruct(ctx).errs
}

func generateSubscriptionID() (string, error) {
	bytes := make([]byte, 32) // 32 bytes = 256 bits
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("__absinthe__:doc:-%s:%s", generateRandomInt64(), strings.ToUpper(hex.EncodeToString(bytes))), nil
}

func generateRandomInt64() string {
	bytes := make([]byte, 8) // 8 bytes = 64 bits
	_, _ = rand.Read(bytes)
	// Convert to int64 and take absolute value
	num := abs(int64(binary.BigEndian.Uint64(bytes)))
	return fmt.Sprintf("%d", num)
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

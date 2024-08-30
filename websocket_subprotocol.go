package phoenix

import (
	"errors"

	"github.com/gorilla/websocket"
)

const (
	phxJoinMessageType messageType = iota
	phxReplyMessageType
	phxLeaveMessageType
	phxErrorMessageType
	phxCloseMessageType
	unsubscribeMessageType
	heartbeatMessageType
	docMessageType
	subscriptionData
	unknownMessageType
)

var (
	errWsConnClosed = errors.New("websocket connection closed")
	errInvalidMsg   = errors.New("invalid message received")
)

type (
	messageType      int
	messageExchanger interface {
		NextMessage() (Message, error)
		Send(m *Message) error
	}
)

func (t messageType) String() string {
	var text string
	switch t {
	default:
		text = "unknown"
	case phxJoinMessageType:
		text = "phx_join"
	case phxReplyMessageType:
		text = "phx_reply"
	case phxLeaveMessageType:
		text = "phx_leave"
	case phxErrorMessageType:
		text = "phx_error"
	case phxCloseMessageType:
		text = "phx_close"
	case unsubscribeMessageType:
		text = "unsubscribe"
	case heartbeatMessageType:
		text = "heartbeat"
	case docMessageType:
		text = "doc"
	case subscriptionData:
		text = "subscription:data"
	}
	return text
}

func handleNextReaderError(err error) error {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
		return errWsConnClosed
	}

	return err
}

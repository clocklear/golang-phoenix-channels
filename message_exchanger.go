package phoenix

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
)

type (
	MessageExchanger struct {
		c *websocket.Conn
	}

	Message struct {
		Ref     *string         `json:"ref"`
		JoinRef *string         `json:"join_ref"`
		Topic   *string         `json:"topic"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}

	phoenixResponse struct {
		Response json.RawMessage `json:"response"`
		Status   string          `json:"status"`
	}

	phoenixSubscriptionResult struct {
		Result         json.RawMessage `json:"result"`
		SubscriptionId string          `json:"subscriptionId,omitempty"`
	}
)

// MarshalJSON overrides the default JSON marshalling
func (m Message) MarshalJSON() ([]byte, error) {
	// Form the payload
	var pl json.RawMessage
	if m.Payload == nil {
		pl = json.RawMessage("{}")
	} else {
		pl = m.Payload
	}

	var b []byte
	var err error
	if m.Event == subscriptionData.String() {
		// This is a subscription data message
		subscriptionId := ""
		if m.Topic != nil {
			subscriptionId = *m.Topic
		}
		// Encode the phoenixSubscriptionResult struct
		resp := phoenixSubscriptionResult{
			Result:         pl,
			SubscriptionId: subscriptionId,
		}
		b, err = json.Marshal(resp)
	} else {
		// Encode the phoenixResponse struct
		resp := phoenixResponse{
			Response: pl,
			Status:   "ok",
		}
		b, err = json.Marshal(resp)
	}
	if err != nil {
		return nil, err
	}
	// Convert the struct to a JSON array
	array := []json.RawMessage{}

	// Subscriptions don't have a joinref in their message
	if m.Event == subscriptionData.String() {
		array = append(array, nil)
	} else {
		array = appendNillableValue(array, m.JoinRef)
	}
	array = appendNillableValue(array, m.Ref)
	array = appendNillableValue(array, m.Topic)
	array = append(array, json.RawMessage(fmt.Sprintf(`"%s"`, m.Event)))
	array = append(array, b)

	return json.Marshal(array)
}

func appendNillableValue(slice []json.RawMessage, val *string) []json.RawMessage {
	if val == nil {
		return append(slice, nil)
	}
	return append(slice, json.RawMessage(fmt.Sprintf(`"%s"`, *val)))
}

func (me MessageExchanger) NextMessage() (Message, error) {
	_, r, err := me.c.NextReader()
	if err != nil {
		return Message{}, handleNextReaderError(err)
	}

	// we create a slice to hold the values of the message
	var i []json.RawMessage
	if err := jsonDecode(r, &i); err != nil {
		return Message{}, errInvalidMsg
	}

	// we can now assign the values to our struct
	var m Message
	_ = json.Unmarshal(i[0], &m.JoinRef)
	_ = json.Unmarshal(i[1], &m.Ref)
	_ = json.Unmarshal(i[2], &m.Topic)
	_ = json.Unmarshal(i[3], &m.Event)
	m.Payload = i[4]

	return m, nil
}

func (me MessageExchanger) Send(m *Message) error {
	return me.c.WriteJSON(m)
}

func (m Message) Type() messageType {
	switch m.Event {
	default:
		return unknownMessageType
	case "phx_join":
		return phxJoinMessageType
	case "doc":
		return docMessageType
	case "phx_leave":
		return phxLeaveMessageType
	case "phx_error":
		return phxErrorMessageType
	case "phx_close":
		return phxCloseMessageType
	case "unsubscribe":
		return unsubscribeMessageType
	case "phx_reply":
		return phxReplyMessageType
	case "heartbeat":
		return heartbeatMessageType
	}
}

func jsonDecode(r io.Reader, val interface{}) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec.Decode(val)
}

// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"fmt"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

// errSessionClosed is returned by every send once the session has ended. The
// socket owns it; this is the name the client has always known it by.
var errSessionClosed = wsconn.ErrSessionClosed

// wireEvent is the provider protocol's event shape.
//
// It is decoded into a struct rather than a map because audio deltas arrive
// tens of times a second per call, and decoding those into maps was measurable
// allocation churn in the reference implementation.
type wireEvent struct {
	Type string `json:"type"`

	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	ItemID     string `json:"item_id"`

	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	Item     *wireItem     `json:"item"`
	Response *wireResponse `json:"response"`
	Error    *wireError    `json:"error"`
}

type wireItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type wireResponse struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Usage  *wireUsage `json:"usage"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

func (e *wireError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// sendEvent encodes one outbound event and writes it.
func (r *Realtime) sendEvent(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbound event: %w", err)
	}
	return r.conn.Send(data)
}

// receive reads and decodes the next event.
func (r *Realtime) receive() (*wireEvent, []byte, error) {
	messageType, data, err := r.conn.Receive()
	if err != nil {
		return nil, nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, data, nil
	}

	var event wireEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, data, fmt.Errorf("decode inbound event: %w", err)
	}
	return &event, data, nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/local/picobot/internal/chat"
)

// MessageTool sends messages to a channel via the chat Hub.
// It holds a context (channel + chatID) which should be set per-incoming-message.
type MessageTool struct {
	hub     *chat.Hub
	channel string
	chatID  string
}

func NewMessageTool(b *chat.Hub) *MessageTool {
	return &MessageTool{hub: b}
}

func (m *MessageTool) Name() string        { return "message" }
func (m *MessageTool) Description() string { return "Send a message to the current channel/chat" }

func (m *MessageTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The message content to send",
			},
		},
		"required": []string{"content"},
	}
}

// SetContext sets the current channel and chat id for outgoing messages.
func (m *MessageTool) SetContext(channel, chatID string) {
	m.channel = channel
	m.chatID = chatID
}

// Expected args: {"content": "..."}
func (m *MessageTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	content := ""
	if c, ok := args["content"]; ok {
		switch v := c.(type) {
		case string:
			content = v
		default:
			b, _ := json.Marshal(v)
			content = string(b)
		}
	}
	if content == "" {
		return "", fmt.Errorf("message tool: 'content' argument required")
	}
	// Publish outbound message to hub
	out := chat.Outbound{
		Channel: m.channel,
		ChatID:  m.chatID,
		Content: content,
	}
	select {
	case m.hub.Out <- out:
		return "sent", nil
	default:
		return "", fmt.Errorf("outbound channel full")
	}
}

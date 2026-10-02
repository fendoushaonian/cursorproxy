package cursor

import (
	"fmt"
	"strings"
	"time"

	"github.com/michael/cursorproxy/internal/pb"
)

// 角色和模式与客户端枚举一致：用户 1、助手 2；Ask 是 1，带工具的 Agent 是 2。
const (
	roleUser      = 1
	roleAssistant = 2
	modeChat      = 1
	modeAgent     = 2
	thinkNone     = 0
	thinkMedium   = 1
	thinkHigh     = 2
)

// Request 是一次要发给 Cursor 的聊天。工具非空时整段会话按 Agent 模式编码。
type Request struct {
	Model     string
	Messages  []Message
	Tools     []Tool
	Reasoning string
	Now       time.Time
	NewID     func() string
}

// Message 是已经摊平的一条对话。Tool 角色的输出放在 ToolResults，不单独占角色值。
type Message struct {
	Role        string
	Content     string
	ToolCallID  string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Tool 是客户端声明的 function tool，编码成 MCP 工具描述。
type Tool struct {
	Name        string
	Description string
	Parameters  string
}

// ToolCall 是助手发起的一次调用。
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// ToolResult 是把工具输出交回模型时挂在消息上的结构。
type ToolResult struct {
	CallID  string
	Name    string
	Index   int
	RawArgs string
	Result  string
}

// BuildBody 生成带 Connect-RPC 帧的请求体。空消息直接拒绝，避免发出去才被上游 400。
func BuildBody(req Request) ([]byte, error) {
	turns, instruction, err := normalize(req)
	if err != nil {
		return nil, err
	}
	id := req.NewID
	if id == nil {
		id = NewUUID
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	agentic := len(req.Tools) > 0
	var chat []byte
	ids := make([]string, len(turns))
	for i := range turns {
		ids[i] = id()
	}
	conv := id()
	for i, turn := range turns {
		msg := encodeMessage(turn, ids[i], agentic)
		chat = pb.AppendBytes(chat, 1, msg)
	}
	chat = pb.AppendVarintField(chat, 2, 1)
	chat = pb.AppendBytes(chat, 3, encodeInstruction(instruction))
	chat = pb.AppendVarintField(chat, 4, 1)
	chat = pb.AppendBytes(chat, 5, encodeModel(req.Model))
	chat = pb.AppendBytes(chat, 8, nil)
	chat = pb.AppendVarintField(chat, 13, 1)
	chat = pb.AppendVarintField(chat, 19, 1)
	chat = pb.AppendString(chat, 23, conv)
	chat = pb.AppendBytes(chat, 26, encodeMetadata(now))
	agenticVal := uint64(0)
	if agentic {
		agenticVal = 1
	}
	chat = pb.AppendVarintField(chat, 27, agenticVal)
	isChat := uint64(1)
	if agentic {
		isChat = 0
		// 29 是枚举，不是嵌套消息。写成字节串时服务端会直接 invalid_argument。
		chat = pb.AppendVarintField(chat, 29, 1)
	}
	chat = pb.AppendVarintField(chat, 22, isChat)
	for i := range turns {
		chat = pb.AppendBytes(chat, 30, encodeMessageID(ids[i]))
	}
	for _, tool := range req.Tools {
		if tool.Name == "" {
			continue
		}
		chat = pb.AppendBytes(chat, 34, encodeTool(tool))
	}
	chat = pb.AppendVarintField(chat, 35, 0)
	chat = pb.AppendVarintField(chat, 38, 0)
	mode := uint64(modeChat)
	modeName := "Ask"
	disable := uint64(1)
	if agentic {
		mode = modeAgent
		modeName = "Agent"
		disable = 0
	}
	chat = pb.AppendVarintField(chat, 46, mode)
	chat = pb.AppendVarintField(chat, 48, disable)
	chat = pb.AppendVarintField(chat, 49, thinkingLevel(req.Reasoning))
	chat = pb.AppendVarintField(chat, 51, 0)
	chat = pb.AppendVarintField(chat, 53, 1)
	chat = pb.AppendString(chat, 54, modeName)

	top := pb.AppendBytes(nil, 1, chat)
	return WrapFrame(0, top), nil
}

// normalize 把 system 收进 instruction。工具输出挂回用户侧消息，
// 同时保留正文，字段 8 被忽略时模型仍能看见结果。
func normalize(req Request) ([]Message, string, error) {
	model := strings.TrimPrefix(strings.TrimSpace(req.Model), "cursor/")
	if model == "" {
		return nil, "", fmt.Errorf("model is empty")
	}
	req.Model = model
	var instruction []string
	var turns []Message
	known := map[string]ToolCall{}
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			if text := strings.TrimSpace(msg.Content); text != "" {
				instruction = append(instruction, text)
			}
		case "user":
			turns = append(turns, Message{Role: "user", Content: msg.Content})
		case "assistant":
			turns = append(turns, Message{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
			for _, call := range msg.ToolCalls {
				if call.ID != "" {
					known[call.ID] = call
				}
			}
		case "tool":
			call := known[msg.ToolCallID]
			name := call.Name
			args := call.Args
			if args == "" {
				args = "{}"
			}
			result := ToolResult{
				CallID:  msg.ToolCallID,
				Name:    name,
				Index:   len(turns),
				RawArgs: args,
				Result:  msg.Content,
			}
			turns = append(turns, Message{Role: "user", Content: msg.Content, ToolResults: []ToolResult{result}})
		default:
			if role == "" {
				continue
			}
			return nil, "", fmt.Errorf("unsupported role %q", msg.Role)
		}
	}
	if len(turns) == 0 {
		return nil, "", fmt.Errorf("messages are empty")
	}
	return turns, strings.Join(instruction, "\n\n"), nil
}

func roleOf(role string) int {
	if role == "assistant" {
		return roleAssistant
	}
	return roleUser
}

func thinkingLevel(effort string) uint64 {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "medium":
		return thinkMedium
	case "high", "xhigh", "max":
		return thinkHigh
	default:
		return thinkNone
	}
}

func encodeMessage(m Message, id string, agentic bool) []byte {
	role := uint64(roleOf(m.Role))
	var b []byte
	b = pb.AppendString(b, 1, m.Content)
	b = pb.AppendVarintField(b, 2, role)
	b = pb.AppendString(b, 13, id)
	for _, tr := range m.ToolResults {
		b = pb.AppendBytes(b, 18, encodeToolResult(tr))
	}
	flag := uint64(0)
	mode := uint64(modeChat)
	if agentic {
		flag = 1
		mode = modeAgent
	}
	b = pb.AppendVarintField(b, 29, flag)
	b = pb.AppendVarintField(b, 47, mode)
	return b
}

func encodeToolResult(tr ToolResult) []byte {
	var b []byte
	b = pb.AppendString(b, 1, tr.CallID)
	b = pb.AppendString(b, 2, tr.Name)
	b = pb.AppendVarintField(b, 3, uint64(tr.Index))
	b = pb.AppendString(b, 5, tr.RawArgs)
	// 字段 8 是结果消息，不是字符串。正文在字段 7，写错线类型会被拒。
	b = pb.AppendString(b, 7, tr.Result)
	return b
}

func encodeInstruction(text string) []byte {
	if text == "" {
		return nil
	}
	return pb.AppendString(nil, 1, text)
}

func encodeModel(name string) []byte {
	return pb.AppendString(nil, 1, name)
}

func encodeMetadata(now time.Time) []byte {
	var b []byte
	b = pb.AppendString(b, 1, clientOS())
	b = pb.AppendString(b, 2, clientArch())
	b = pb.AppendString(b, 3, "v22.14.0")
	b = pb.AppendString(b, 4, "/")
	b = pb.AppendString(b, 5, now.UTC().Format("2006-01-02T15:04:05.000Z"))
	// 字段 7 是客户端版本。缺了或只有两段时，上游会把版本当成低于 2.4.0。
	b = pb.AppendString(b, 7, clientVersion)
	return b
}

func encodeMessageID(id string) []byte {
	// 只写 bubble id。头部的 type 枚举和消息角色不是同一张表，乱填会被拒。
	return pb.AppendString(nil, 1, id)
}

func encodeTool(t Tool) []byte {
	var b []byte
	b = pb.AppendString(b, 1, t.Name)
	if t.Description != "" {
		b = pb.AppendString(b, 2, t.Description)
	}
	if t.Parameters != "" {
		b = pb.AppendString(b, 3, t.Parameters)
	}
	b = pb.AppendString(b, 4, "custom")
	return b
}

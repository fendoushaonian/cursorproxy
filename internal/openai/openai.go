// Package openai 把本地的 OpenAI Chat Completions 转成 Cursor 对话。
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/michael/cursorproxy/internal/cursor"
)

// Server 是 OpenAI 兼容的本地入口。Token 来自构造参数，不接受客户端自带的上游密钥。
type Server struct {
	Client *cursor.Client
	Creds  cursor.Credentials
	Models []string
}

// New 返回可直接交给 http.Server 的处理函数。上游登录态只来自 creds。
func New(client *cursor.Client, creds cursor.Credentials) http.Handler {
	return (&Server{Client: client, Creds: creds}).Handler()
}

// Handler 挂上 /v1/chat/completions、/v1/models 和 /healthz。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]item, 0, len(s.modelIDs()))
	for _, id := range s.modelIDs() {
		data = append(data, item{ID: id, Object: "model", Created: now, OwnedBy: "cursor"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) modelIDs() []string {
	if len(s.Models) > 0 {
		return s.Models
	}
	return []string{"composer-2.5", "composer-2"}
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	Tools           []tool    `json:"tools"`
	Stream          bool      `json:"stream"`
	ReasoningEffort string    `json:"reasoning_effort"`
}

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCallID string     `json:"tool_call_id"`
	ToolCalls  []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	messages, err := toCursorMessages(req.Messages)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	creds := s.Creds
	if req.ReasoningEffort != "" {
		creds.Reasoning = req.ReasoningEffort
	}
	body, err := s.client().Stream(r.Context(), creds, req.Model, messages, toCursorTools(req.Tools))
	if err != nil {
		writeError(w, statusOf(err), "upstream_error", err.Error())
		return
	}
	defer body.Close()
	if req.Stream {
		s.writeStream(w, r, req.Model, body)
		return
	}
	s.writeOnce(w, req.Model, body)
}

func (s *Server) client() *cursor.Client {
	if s.Client != nil {
		return s.Client
	}
	return &cursor.Client{}
}

func (s *Server) writeOnce(w http.ResponseWriter, model string, body io.Reader) {
	events, err := cursor.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	text, reasoning, calls, upErr := fold(events)
	if upErr != nil && text == "" && len(calls) == 0 {
		writeError(w, http.StatusBadGateway, "upstream_error", upErr.Error())
		return
	}
	msg := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	finish := "stop"
	if len(calls) > 0 {
		msg["tool_calls"] = calls
		finish = "tool_calls"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + cursorID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	})
}

func (s *Server) writeStream(w http.ResponseWriter, r *http.Request, model string, body io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	id := "chatcmpl-" + cursorID()
	created := time.Now().Unix()
	var calls []map[string]any
	finish := "stop"
	err := cursor.ReadFrames(body, func(ev cursor.Event) error {
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		switch ev.Kind {
		case cursor.EventText:
			writeChunk(w, id, created, model, map[string]any{"content": ev.Text}, nil)
		case cursor.EventThinking:
			writeChunk(w, id, created, model, map[string]any{"reasoning_content": ev.Text}, nil)
		case cursor.EventToolCall:
			idx := len(calls)
			calls = append(calls, toolCallJSON(ev.Tool))
			writeChunk(w, id, created, model, map[string]any{}, []any{streamTool(idx, ev.Tool)})
			finish = "tool_calls"
		case cursor.EventError:
			writeSSE(w, map[string]any{"error": map[string]string{"message": ev.Err.Error(), "type": "upstream_error"}})
			flusher.Flush()
			return ev.Err
		case cursor.EventEnd:
			return nil
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeSSE(w, map[string]any{"error": map[string]string{"message": err.Error(), "type": "upstream_error"}})
		flusher.Flush()
		return
	}
	writeChunk(w, id, created, model, map[string]any{}, nil, finish)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeChunk(w http.ResponseWriter, id string, created int64, model string, delta map[string]any, tools []any, finish ...string) {
	if tools != nil {
		delta["tool_calls"] = tools
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if len(finish) > 0 {
		choice["finish_reason"] = finish[0]
	} else {
		choice["finish_reason"] = nil
	}
	writeSSE(w, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{choice},
	})
}

func writeSSE(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func fold(events []cursor.Event) (text, reasoning string, calls []map[string]any, err error) {
	var b strings.Builder
	var t strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case cursor.EventText:
			b.WriteString(ev.Text)
		case cursor.EventThinking:
			t.WriteString(ev.Text)
		case cursor.EventToolCall:
			calls = append(calls, toolCallJSON(ev.Tool))
		case cursor.EventError:
			err = ev.Err
		}
	}
	return b.String(), t.String(), calls, err
}

func toolCallJSON(call cursor.ToolCall) map[string]any {
	args := call.Args
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"id":   call.ID,
		"type": "function",
		"function": map[string]string{
			"name":      call.Name,
			"arguments": args,
		},
	}
}

func streamTool(index int, call cursor.ToolCall) map[string]any {
	args := call.Args
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"index": index,
		"id":    call.ID,
		"type":  "function",
		"function": map[string]string{
			"name":      call.Name,
			"arguments": args,
		},
	}
}

func toCursorMessages(in []message) ([]cursor.Message, error) {
	if len(in) == 0 {
		return nil, errors.New("messages is empty")
	}
	out := make([]cursor.Message, 0, len(in))
	for _, m := range in {
		cm := cursor.Message{Role: m.Role, Content: contentText(m), ToolCallID: m.ToolCallID}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				cm.ToolCalls = append(cm.ToolCalls, cursor.ToolCall{
					ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments,
				})
			}
		}
		out = append(out, cm)
	}
	return out, nil
}

func contentText(m message) string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			obj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if obj["type"] == "text" {
				if s, ok := obj["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		if m.Role == "tool" && m.Content != nil {
			b, _ := json.Marshal(m.Content)
			return string(b)
		}
		return ""
	}
}

func toCursorTools(in []tool) []cursor.Tool {
	out := make([]cursor.Tool, 0, len(in))
	for _, t := range in {
		name := t.Function.Name
		if name == "" {
			continue
		}
		out = append(out, cursor.Tool{
			Name: name, Description: t.Function.Description, Parameters: string(t.Function.Parameters),
		})
	}
	return out
}

func statusOf(err error) int {
	var up *cursor.UpstreamError
	if errors.As(err, &up) {
		switch up.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return http.StatusBadGateway
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests
		default:
			if up.Status >= 400 && up.Status < 600 {
				return http.StatusBadGateway
			}
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "access token is empty") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": typ},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func cursorID() string {
	return strings.ReplaceAll(time.Now().Format("20060102150405.000"), ".", "")
}

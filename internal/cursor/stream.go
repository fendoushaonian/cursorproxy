package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxFrame = 32 << 20

// Delta 是一帧里解出来的增量。思考和正文分开，避免混进用户可见的回答。
type Delta struct {
	Text     string
	Thinking string
	Tool     *ToolCall
}

// UpstreamError 是 Cursor 用 HTTP 状态或帧内 JSON 返回的失败。
type UpstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	if e.Status != 0 {
		return fmt.Sprintf("cursor http %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// WrapFrame 在载荷前加上 1 字节标志和 4 字节大端长度。请求本身不压缩。
func WrapFrame(flags byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// NextFrame 从缓冲区切出一帧。数据不够时 ok 为 false 且不消费；长度或标志非法则返回错误。
func NextFrame(buf []byte) (flags byte, payload, rest []byte, ok bool, err error) {
	if len(buf) < 5 {
		return 0, nil, buf, false, nil
	}
	flags = buf[0]
	if flags&^0x03 != 0 {
		return 0, nil, buf, false, fmt.Errorf("unknown connect flags 0x%02x", flags)
	}
	n := int(binary.BigEndian.Uint32(buf[1:5]))
	if n < 0 || n > maxFrame {
		return 0, nil, buf, false, fmt.Errorf("connect frame length %d", n)
	}
	if len(buf) < 5+n {
		return 0, nil, buf, false, nil
	}
	payload = buf[5 : 5+n]
	rest = buf[5+n:]
	if flags&0x01 != 0 {
		decoded, gunzipErr := gunzip(payload)
		if gunzipErr != nil {
			return 0, nil, buf, false, fmt.Errorf("gunzip frame: %w", gunzipErr)
		}
		payload = decoded
	}
	return flags, payload, rest, true, nil
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxFrame))
}

// DecodePayload 解析一帧业务载荷。结束帧和 `{` 开头的 JSON 只用来识别错误，不当成正文。
func DecodePayload(flags byte, payload []byte) ([]Delta, error) {
	if flags&0x02 != 0 || (len(payload) > 0 && payload[0] == '{') {
		if err := errorFromJSON(payload); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if len(payload) == 0 {
		return nil, nil
	}
	fields, err := pbFields(payload)
	if err != nil {
		return nil, err
	}
	var out []Delta
	for _, raw := range fields[1] {
		call, callErr := extractToolCall(raw)
		if callErr != nil {
			return nil, callErr
		}
		if call != nil {
			out = append(out, Delta{Tool: call})
		}
	}
	for _, raw := range fields[2] {
		d, decErr := extractChat(raw)
		if decErr != nil {
			return nil, decErr
		}
		if d.Text != "" || d.Thinking != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

func errorFromJSON(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return nil
	}
	if body.Error.Code == "" && body.Error.Message == "" {
		return nil
	}
	status := 502
	if body.Error.Code == "resource_exhausted" || body.Error.Code == "rate_limit_exceeded" {
		status = 429
	}
	if body.Error.Code == "unauthenticated" {
		status = 401
	}
	msg := body.Error.Message
	if msg == "" || msg == "Error" {
		raw := strings.TrimSpace(string(payload))
		if len(raw) > 400 {
			raw = raw[:400]
		}
		if raw != "" {
			msg = raw
		} else if msg == "" {
			msg = body.Error.Code
		}
	}
	return &UpstreamError{Status: status, Code: body.Error.Code, Message: msg}
}

func extractChat(raw []byte) (Delta, error) {
	fields, err := pbFields(raw)
	if err != nil {
		return Delta{}, err
	}
	var d Delta
	if text := fields[1]; len(text) > 0 {
		d.Text = string(text[0])
	}
	if thinking := fields[25]; len(thinking) > 0 {
		inner, err := pbFields(thinking[0])
		if err != nil {
			return Delta{}, err
		}
		if t := inner[1]; len(t) > 0 {
			d.Thinking = string(t[0])
		}
	}
	return d, nil
}

func extractToolCall(raw []byte) (*ToolCall, error) {
	fields, err := pbFields(raw)
	if err != nil {
		return nil, err
	}
	call := &ToolCall{}
	if v := fields[3]; len(v) > 0 {
		call.ID = string(v[0])
	}
	if v := fields[9]; len(v) > 0 {
		call.Name = string(v[0])
	}
	if v := fields[10]; len(v) > 0 {
		call.Args = string(v[0])
	}
	if call.Name == "" {
		if nested := fields[27]; len(nested) > 0 {
			params, err := pbFields(nested[0])
			if err != nil {
				return nil, err
			}
			for _, item := range params[1] {
				one, err := pbFields(item)
				if err != nil {
					return nil, err
				}
				if call.Name == "" && len(one[1]) > 0 {
					call.Name = string(one[1][0])
				}
				if len(one[3]) > 0 {
					call.Args = string(one[3][0])
				}
			}
		}
	}
	if call.ID == "" && call.Name == "" && call.Args == "" {
		return nil, nil
	}
	if call.Args == "" {
		call.Args = "{}"
	}
	return call, nil
}

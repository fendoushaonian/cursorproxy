package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/michael/cursorproxy/internal/pb"
)

const (
	defaultBaseURL = "https://api2.cursor.sh"
	// 3.12 的聊天是两步：BidiAppend 送正文，SSE 只带请求号把流拉回来。
	// 直接把聊天体 POST 到旧的双向接口会被判 invalid_argument。
	appendPath    = "/aiserver.v1.BidiService/BidiAppend"
	chatPath      = "/aiserver.v1.ChatService/StreamUnifiedChatWithToolsSSE"
	clientVersion = "3.23.12"
)

// Credentials 是一次上游调用用的登录态。MachineID 为空时由 token 派生。
// MacMachineID 非空时写进校验和，3.12 客户端在 macOS 上总会带上它。
type Credentials struct {
	AccessToken  string
	MachineID    string
	MacMachineID string
	OS           string
	Arch         string
	GhostMode    bool
	Reasoning    string
}

// Client 把一次对话发到 Cursor 的 StreamUnifiedChatWithTools。
// Base 为空时用 api2.cursor.sh。HTTP 为空时用带超时的默认客户端。
type Client struct {
	Base    string
	HTTP    *http.Client
	Version string
}

// Stream 把 messages 编成 Connect 帧并 POST 上去。
// 调用方必须读完或关闭返回的 body。非 2xx 时 body 已被读完并关闭。
func (c *Client) Stream(ctx context.Context, creds Credentials, model string, messages []Message, tools []Tool) (io.ReadCloser, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access token is empty")
	}
	framed, err := BuildBody(Request{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		Reasoning: creds.Reasoning,
	})
	if err != nil {
		return nil, err
	}
	if len(framed) < 5 {
		return nil, fmt.Errorf("cursor: chat frame is empty")
	}
	payload := framed[5:]
	requestID := NewUUID()
	// BidiAppend 是一元 RPC，正文是裸 protobuf，Content-Type 必须是 application/proto。
	// 套上 Connect 帧会被 415。后面的 SSE 才是流，继续用带帧的 connect+proto。
	if err := c.post(ctx, creds, appendPath, encodeAppend(requestID, payload)); err != nil {
		return nil, err
	}
	return c.open(ctx, creds, chatPath, WrapFrame(0, pb.AppendString(nil, 1, requestID)))
}

// encodeAppend 把聊天 protobuf 交给 BidiAppend。
// 客户端二选一：默认把原文做成十六进制放进 data；功能开关打开才改用 data_binary。
// 两个一起写时，拉流那一侧解出来的不是合法消息，会回 invalid_argument。
func encodeAppend(requestID string, payload []byte) []byte {
	var b []byte
	b = pb.AppendString(b, 1, hex.EncodeToString(payload))
	b = pb.AppendBytes(b, 2, pb.AppendString(nil, 1, requestID))
	b = pb.AppendVarintField(b, 3, 0)
	return b
}

func (c *Client) post(ctx context.Context, creds Credentials, path string, body []byte) error {
	resp, err := c.do(ctx, creds, path, body, "application/proto")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamStatus(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) open(ctx context.Context, creds Credentials, path string, body []byte) (io.ReadCloser, error) {
	resp, err := c.do(ctx, creds, path, body, "application/connect+proto")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, upstreamStatus(resp)
	}
	// 调用方取消后必须拆掉这条还在读的连接。只关 body 时服务端仍停在
	// StateActive，httptest 和真实上游都会一直等到读完。
	stop := context.AfterFunc(ctx, func() { resp.Body.Close() })
	return &cancelBody{ReadCloser: resp.Body, stop: stop}, nil
}

func (c *Client) do(ctx context.Context, creds Credentials, path string, body []byte, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range c.headers(creds) {
		req.Header.Set(k, v)
	}
	req.Header.Set("content-type", contentType)
	return c.http().Do(req)
}

func upstreamStatus(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("cursor: upstream %d: %s", resp.StatusCode, msg)
}

// cancelBody 在读完或调用方关闭时停掉取消回调，避免请求结束后再关一次。
type cancelBody struct {
	io.ReadCloser
	stop func() bool
	once sync.Once
}

func (b *cancelBody) Close() error {
	b.once.Do(func() {
		if b.stop != nil {
			b.stop()
		}
	})
	return b.ReadCloser.Close()
}

// Chat 完成一次非流式调用：编码、POST、把全部帧解成事件。
func (c *Client) Chat(ctx context.Context, creds Credentials, req Request) ([]Event, error) {
	if creds.Reasoning == "" {
		creds.Reasoning = req.Reasoning
	}
	body, err := c.Stream(ctx, creds, req.Model, req.Messages, req.Tools)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return ReadAll(body)
}

func (c *Client) url(path string) string {
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = defaultBaseURL
	}
	return base + path
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (c *Client) headers(creds Credentials) map[string]string {
	version := c.Version
	if version == "" {
		version = clientVersion
	}
	token := CleanToken(creds.AccessToken)
	machine := creds.MachineID
	if machine == "" {
		machine = Hash64(token, "machineId")
	}
	osName := creds.OS
	if osName == "" {
		osName = clientOS()
	}
	arch := creds.Arch
	if arch == "" {
		arch = clientArch()
	}
	h := Headers(token, machine, creds.MacMachineID, "Asia/Shanghai", creds.GhostMode)
	h["x-cursor-client-version"] = version
	h["x-cursor-client-os"] = osName
	h["x-cursor-client-arch"] = arch
	return h
}

// EventKind 区分正文、思考、工具调用和上游错误。
type EventKind int

const (
	EventText EventKind = iota
	EventThinking
	EventToolCall
	EventError
	EventEnd
)

// Event 是从一帧里拆出来的一条增量。Err 只在 EventError 时有值。
type Event struct {
	Kind EventKind
	Text string
	Tool ToolCall
	Err  error
}

// 一帧里可能有多条增量，而 ReadEvent 的签名没有解码器对象。
// 剩余事件按 reader 暂存，EOF 或出错时清掉，避免流式接口丢掉同帧里的工具调用。
var (
	pendingMu sync.Mutex
	pending   = map[*bufio.Reader][]Event{}
)

// ReadEvent 读下一条增量。流结束返回 io.EOF。半截帧返回错误，不当作正常结束。
func ReadEvent(r *bufio.Reader) (Event, error) {
	if ev, ok := popPending(r); ok {
		return ev, nil
	}
	for {
		evs, err := readOneFrame(r)
		if err != nil {
			clearPending(r)
			return Event{}, err
		}
		if len(evs) == 0 {
			continue
		}
		if len(evs) > 1 {
			setPending(r, evs[1:])
		}
		return evs[0], nil
	}
}

// ReadFrames 按顺序交出每一条增量。buf 在半截帧处结束时返回错误。
func ReadFrames(r io.Reader, fn func(Event) error) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	for {
		evs, err := readOneFrame(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
}

// ReadAll 把整段响应读成事件。适合非流式接口。
func ReadAll(r io.Reader) ([]Event, error) {
	var out []Event
	err := ReadFrames(r, func(ev Event) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

func readOneFrame(r *bufio.Reader) ([]Event, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}
	n := int(header[1])<<24 | int(header[2])<<16 | int(header[3])<<8 | int(header[4])
	if n > maxFrame {
		return nil, fmt.Errorf("connect frame length %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	frame := make([]byte, 0, 5+n)
	frame = append(frame, header...)
	frame = append(frame, payload...)
	flags, decoded, _, ok, err := NextFrame(frame)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	deltas, err := DecodePayload(flags, decoded)
	if err != nil {
		var ue *UpstreamError
		if errors.As(err, &ue) {
			return []Event{{Kind: EventError, Err: ue}}, nil
		}
		return nil, err
	}
	events := make([]Event, 0, len(deltas))
	for _, d := range deltas {
		switch {
		case d.Tool != nil:
			events = append(events, Event{Kind: EventToolCall, Tool: *d.Tool})
		case d.Thinking != "":
			events = append(events, Event{Kind: EventThinking, Text: d.Thinking})
		case d.Text != "":
			events = append(events, Event{Kind: EventText, Text: d.Text})
		}
	}
	if len(events) == 0 && flags&0x02 != 0 {
		return []Event{{Kind: EventEnd}}, nil
	}
	return events, nil
}

func popPending(r *bufio.Reader) (Event, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	q := pending[r]
	if len(q) == 0 {
		return Event{}, false
	}
	ev := q[0]
	if len(q) == 1 {
		delete(pending, r)
	} else {
		pending[r] = q[1:]
	}
	return ev, true
}

func setPending(r *bufio.Reader, evs []Event) {
	pendingMu.Lock()
	pending[r] = evs
	pendingMu.Unlock()
}

func clearPending(r *bufio.Reader) {
	pendingMu.Lock()
	delete(pending, r)
	pendingMu.Unlock()
}

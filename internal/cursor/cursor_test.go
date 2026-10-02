// cursor_test 锁住请求编码和响应切帧：字段号、gzip 帧、Jyh 校验和。
// 这些值没有 .proto，对不上公开客户端实现时上游会直接拒。
package cursor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael/cursorproxy/internal/pb"
)

func TestChecksumShape(t *testing.T) {
	sum := ChecksumAt(1_700_000_000_000, "machine", "")
	if !strings.HasSuffix(sum, "machine") {
		t.Fatalf("checksum = %q", sum)
	}
	prefix := strings.TrimSuffix(sum, "machine")
	if prefix == "" {
		t.Fatal("empty prefix")
	}
	for _, c := range prefix {
		if !strings.ContainsRune(urlSafeAlphabet, c) {
			t.Fatalf("prefix %q has %q", prefix, c)
		}
	}
	if ChecksumAt(1_700_000_000_000, "m", "") != ChecksumAt(1_700_000_000_000, "m", "") {
		t.Fatal("same timestamp must be stable")
	}
	// 客户端把毫秒除以 1e6，同一窗口内的时间戳必须得到同一个前缀。
	if ChecksumAt(1_700_000_000_000, "m", "") != ChecksumAt(1_700_000_000_000+999_999, "m", "") {
		t.Fatal("timestamp window must be 1e6 ms")
	}
	mac := ChecksumAt(1_700_000_000_000, "machine", "macid")
	if !strings.HasSuffix(mac, "machine/macid") {
		t.Fatalf("mac checksum = %q", mac)
	}
}

func TestCleanToken(t *testing.T) {
	if got := CleanToken("user_01::jwt"); got != "jwt" {
		t.Fatalf("clean = %q", got)
	}
	if got := CleanToken("jwt"); got != "jwt" {
		t.Fatalf("plain = %q", got)
	}
}

func TestHeaders(t *testing.T) {
	h := Headers("user::tok", "mid", "mac", "Asia/Shanghai", false)
	if h["authorization"] != "Bearer tok" {
		t.Fatal(h["authorization"])
	}
	if !strings.HasSuffix(h["x-cursor-checksum"], "mid/mac") {
		t.Fatal(h["x-cursor-checksum"])
	}
	if h["x-cursor-client-version"] != "3.23.12" {
		t.Fatal(h["x-cursor-client-version"])
	}
	if h["content-type"] != "application/connect+proto" {
		t.Fatal(h["content-type"])
	}
	if Headers("tok", "", "", "", true)["x-ghost-mode"] != "true" {
		t.Fatal("ghost")
	}
}

func TestBuildBodyRoundTrip(t *testing.T) {
	body, err := BuildBody(Request{
		Model: "composer-1",
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
			{Role: "tool", ToolCallID: "c1", Content: "file"},
		},
		Tools: []Tool{{
			Name: "read", Description: "read a file",
			Parameters: `{"type":"object"}`,
		}},
		Reasoning: "high",
		NewID:     func() string { return "id" },
	})
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.BigEndian.Uint32(body[1:5]))
	if body[0] != 0 || 5+n != len(body) {
		t.Fatalf("frame flags=%d len=%d body=%d", body[0], n, len(body))
	}
	top, err := pb.Parse(body[5:])
	if err != nil {
		t.Fatal(err)
	}
	var inner []byte
	for _, f := range top {
		if f.Num == 1 && f.Wire == pb.WireBytes {
			inner = f.Bytes
		}
	}
	if len(inner) == 0 {
		t.Fatal("missing request field")
	}
	fields, err := pb.Parse(inner)
	if err != nil {
		t.Fatal(err)
	}
	var model string
	for _, f := range fields {
		if f.Num != 5 {
			continue
		}
		mf, err := pb.Parse(f.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mf {
			if m.Num == 1 {
				model = string(m.Bytes)
			}
		}
	}
	if model != "composer-1" {
		t.Fatalf("model = %q", model)
	}
}

func TestDecodeTextThinkingAndTool(t *testing.T) {
	text := pb.AppendBytes(nil, 2, pb.AppendString(nil, 1, "hello"))
	thinking := pb.AppendBytes(nil, 2, pb.AppendBytes(nil, 25, pb.AppendString(nil, 1, "hmm")))
	call := pb.AppendBytes(nil, 1, join(
		pb.AppendString(nil, 3, "call_1"),
		pb.AppendString(nil, 9, "shell"),
		pb.AppendString(nil, 10, `{"cmd":"ls"}`),
	))
	raw := join(WrapFrame(0, text), WrapFrame(0, thinking), WrapFrame(0, call))

	var got []Event
	err := ReadFrames(bytes.NewReader(raw), func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != EventText || got[0].Text != "hello" {
		t.Fatalf("%+v", got)
	}
	if got[1].Kind != EventThinking || got[1].Text != "hmm" {
		t.Fatalf("thinking %+v", got[1])
	}
	if got[2].Kind != EventToolCall || got[2].Tool.Name != "shell" || got[2].Tool.Args != `{"cmd":"ls"}` {
		t.Fatalf("tool %+v", got[2])
	}
}

func TestDecodeGzipAndEndError(t *testing.T) {
	payload := pb.AppendBytes(nil, 2, pb.AppendString(nil, 1, "gz"))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	end := WrapFrame(0x02, []byte(`{"error":{"code":"resource_exhausted","message":"nope"}}`))
	raw := join(WrapFrame(0x01, buf.Bytes()), end)

	var got []Event
	if err := ReadFrames(bytes.NewReader(raw), func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ue, ok := got[1].Err.(*UpstreamError)
	if len(got) != 2 || got[0].Text != "gz" || !ok || ue.Message != "nope" {
		t.Fatalf("%+v", got)
	}
	if err := ReadFrames(bytes.NewReader([]byte{0, 0, 0, 0, 9, 1}), func(Event) error { return nil }); err == nil {
		t.Fatal("expected truncated frame")
	}
}

func TestChatUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer tok" {
			t.Errorf("auth %s", r.Header.Get("authorization"))
		}
		if !strings.Contains(r.URL.Path, "BidiAppend") && !strings.Contains(r.URL.Path, "StreamUnifiedChat") {
			t.Errorf("path %s", r.URL.Path)
		}
		ct := r.Header.Get("content-type")
		if ct != "application/proto" && ct != "application/connect+proto" {
			t.Errorf("type %s", ct)
		}
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	_, err := c.Chat(context.Background(), Credentials{AccessToken: "tok"}, Request{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal(err)
	}
}

func TestChatEmptyMessages(t *testing.T) {
	c := &Client{Base: "http://127.0.0.1"}
	_, err := c.Chat(context.Background(), Credentials{AccessToken: "t"}, Request{Model: "m"})
	if err == nil {
		t.Fatal("expected empty message error")
	}
}

func TestChatDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		text := pb.AppendBytes(nil, 2, pb.AppendString(nil, 1, "pong"))
		_, _ = w.Write(WrapFrame(0, text))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	events, err := c.Chat(context.Background(), Credentials{AccessToken: "tok"}, Request{
		Model: "composer-1", Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "pong" {
		t.Fatalf("%+v", events)
	}
}

func join(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

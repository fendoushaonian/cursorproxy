// openai_test 用 httptest 冒充 api2，确认 OpenAI 形状的请求能编成 Connect 帧，
// 流式响应能按 SSE 吐回。不打真实 Cursor。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael/cursorproxy/internal/cursor"
	"github.com/michael/cursorproxy/internal/pb"
)

func TestChatCompletionAggregates(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := pb.AppendBytes(nil, 2, pb.AppendString(nil, 1, "pong"))
		_, _ = w.Write(cursor.WrapFrame(0, text))
	}))
	defer up.Close()

	h := New(cursorClient(up), cursor.Credentials{AccessToken: "tok"})
	body := `{"model":"composer-1","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "pong" {
		t.Fatalf("%v", msg)
	}
}

func TestStreamSSE(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := pb.AppendBytes(nil, 2, pb.AppendString(nil, 1, "hi"))
		call := bytes.Join([][]byte{
			pb.AppendString(nil, 3, "c1"),
			pb.AppendString(nil, 9, "shell"),
			pb.AppendString(nil, 10, `{"a":1}`),
		}, nil)
		_, _ = w.Write(cursor.WrapFrame(0, text))
		_, _ = w.Write(cursor.WrapFrame(0, pb.AppendBytes(nil, 1, call)))
	}))
	defer up.Close()

	h := New(cursorClient(up), cursor.Credentials{AccessToken: "tok"})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %s body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"content":"hi"`) || !strings.Contains(raw, `"name":"shell"`) || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("sse:\n%s", raw)
	}
}

func TestModelsAndHealth(t *testing.T) {
	h := New(&cursor.Client{Base: "http://127.0.0.1"}, cursor.Credentials{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "composer-2.5") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatal(rec.Body.String())
	}
}

func TestMissingMessages(t *testing.T) {
	h := New(&cursor.Client{Base: "http://127.0.0.1"}, cursor.Credentials{AccessToken: "t"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatal(rec.Code)
	}
}

func TestUpstreamUnauthorized(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer up.Close()
	h := New(cursorClient(up), cursor.Credentials{AccessToken: "tok"})
	body := `{"model":"m","messages":[{"role":"user","content":"x"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
}

func TestContextCancelDuringStream(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 正文读完后服务端才在后台等连接关闭，并据此取消 r.Context。
		// 不读的话客户端断开也不会唤醒这里，httptest.Close 会一直等。
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer up.Close()
	h := New(cursorClient(up), cursor.Credentials{AccessToken: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	go func() {
		<-started
		cancel()
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
}

func cursorClient(up *httptest.Server) *cursor.Client {
	return &cursor.Client{Base: up.URL, HTTP: up.Client()}
}

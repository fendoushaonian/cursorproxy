// Command cursorproxy 在本机提供 OpenAI 兼容接口，并把对话转到 Cursor。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/michael/cursorproxy/internal/cursor"
	"github.com/michael/cursorproxy/internal/openai"
)

// version 是这个反代的发布号，和本机 Cursor 客户端版本不是一回事。
const version = "0.1.0"

func main() {
	addr := flag.String("addr", env("CURSORPROXY_ADDR", "127.0.0.1:8787"), "listen address")
	base := flag.String("upstream", env("CURSOR_BASE_URL", "https://api2.cursor.sh"), "Cursor API base URL")
	token := flag.String("token", env("CURSOR_ACCESS_TOKEN", ""), "Cursor access token; userId::jwt is accepted")
	machine := flag.String("machine-id", env("CURSOR_MACHINE_ID", ""), "machine id; derived from the token when empty")
	models := flag.String("models", env("CURSOR_MODELS", "composer-2.5,composer-2"), "comma-separated model ids")
	flag.Parse()

	accessToken := strings.TrimSpace(*token)
	machineID := strings.TrimSpace(*machine)
	var macID string
	if accessToken == "" || machineID == "" {
		local, err := cursor.ReadLocalAuth("")
		if err != nil && accessToken == "" {
			log.Fatalf("没有本机 Cursor 登录态，也没有 CURSOR_ACCESS_TOKEN: %v", err)
		}
		if accessToken == "" {
			accessToken = local.AccessToken
		}
		if machineID == "" {
			machineID = local.MachineID
			macID = local.MacMachineID
		}
		log.Printf("已读取本机 Cursor 登录态")
	}
	srv := &openai.Server{
		Client: &cursor.Client{Base: *base, Version: "3.23.12"},
		Creds: cursor.Credentials{
			AccessToken:  accessToken,
			MachineID:    machineID,
			MacMachineID: macID,
		},
		Models: split(*models),
	}
	log.Printf("cursorproxy %s listening on http://%s", version, *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func split(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

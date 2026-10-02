// Package cursor 把 OpenAI 聊天请求转成 Cursor IDE 的 Connect-RPC 调用。
// 字段号没有公开的 proto，与客户端抓包和已公开的移植（同一套 FIELD 表）对齐。
package cursor

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"time"
)

// urlSafeAlphabet 是校验和不加填充的 URL-safe base64 字母表，与客户端手写编码器一致。
const urlSafeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// dnsNamespace 是 RFC 4122 的 DNS UUID 名字空间，session id 用它做 UUID v5。
var dnsNamespace = []byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// Hash64 返回 sha256(input+salt) 的 64 位十六进制。machineId 和 x-client-key 都这么派生。
func Hash64(input, salt string) string {
	sum := sha256.Sum256([]byte(input + salt))
	return hex.EncodeToString(sum[:])
}

// Checksum 生成当前时刻的 x-cursor-checksum。3.12 起有 Mac 机器码时格式是
// base64(timestamp) + machineId + "/" + macMachineId，没有则不带斜杠。
func Checksum(machineID, macMachineID string) string {
	return ChecksumAt(time.Now().UnixMilli(), machineID, macMachineID)
}

// ChecksumAt 用给定的 Unix 毫秒生成校验和。时间戳先除以 1e6，约每 16 分钟才变一次，
// 这是客户端的粒度，不是笔误。
func ChecksumAt(unixMilli int64, machineID, macMachineID string) string {
	timestamp := unixMilli / 1000000
	raw := []byte{
		byte((timestamp >> 40) & 0xFF),
		byte((timestamp >> 32) & 0xFF),
		byte((timestamp >> 24) & 0xFF),
		byte((timestamp >> 16) & 0xFF),
		byte((timestamp >> 8) & 0xFF),
		byte(timestamp & 0xFF),
	}
	t := byte(165)
	for i := range raw {
		raw[i] = (raw[i] ^ t) + byte(i%256)
		t = raw[i]
	}
	sum := encodeBase64URL(raw) + machineID
	if macMachineID != "" {
		sum += "/" + macMachineID
	}
	return sum
}

func encodeBase64URL(b []byte) string {
	out := make([]byte, 0, (len(b)*4+2)/3)
	for i := 0; i < len(b); i += 3 {
		a := b[i]
		var bb, cc byte
		if i+1 < len(b) {
			bb = b[i+1]
		}
		if i+2 < len(b) {
			cc = b[i+2]
		}
		out = append(out, urlSafeAlphabet[a>>2])
		out = append(out, urlSafeAlphabet[((a&3)<<4)|(bb>>4)])
		if i+1 < len(b) {
			out = append(out, urlSafeAlphabet[((bb&15)<<2)|(cc>>6)])
		}
		if i+2 < len(b) {
			out = append(out, urlSafeAlphabet[cc&63])
		}
	}
	return string(out)
}

// CleanToken 去掉 Cursor 登录串里 userId:: 前缀，线上只认后面的 JWT。
func CleanToken(token string) string {
	for i := 0; i+1 < len(token); i++ {
		if token[i] == ':' && token[i+1] == ':' {
			return token[i+2:]
		}
	}
	return token
}

// Headers 组装一次上游请求需要的全部头。machineID 为空时用 token 派生，换机器不会漂。
func Headers(accessToken, machineID, macMachineID, timezone string, ghost bool) map[string]string {
	clean := CleanToken(accessToken)
	if machineID == "" {
		machineID = Hash64(clean, "machineId")
	}
	if timezone == "" {
		timezone = "UTC"
	}
	ghostVal := "false"
	if ghost {
		ghostVal = "true"
	}
	return map[string]string{
		"authorization":               "Bearer " + clean,
		"connect-accept-encoding":     "gzip",
		"connect-protocol-version":    "1",
		"content-type":                "application/connect+proto",
		"user-agent":                  "connect-es/1.6.1",
		"x-amzn-trace-id":             "Root=" + NewUUID(),
		"x-client-key":                Hash64(clean, ""),
		"x-cursor-checksum":           Checksum(machineID, macMachineID),
		"x-cursor-client-version":     "3.23.12",
		"x-cursor-client-type":        "ide",
		"x-cursor-client-os":          clientOS(),
		"x-cursor-client-arch":        clientArch(),
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":     NewUUID(),
		"x-cursor-timezone":           timezone,
		"x-ghost-mode":                ghostVal,
		"x-request-id":                NewUUID(),
		"x-session-id":                uuidV5(dnsNamespace, []byte(clean)),
	}
}

func clientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	default:
		return runtime.GOOS
	}
}

func clientArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// NewUUID 返回随机 UUID v4。只用于请求关联，不作为安全凭证。
func NewUUID() string {
	var b [16]byte
	if _, err := cryptoRand(b[:]); err != nil {
		// 读不到系统随机数时用时间撑住唯一性，避免整次请求因为 id 失败。
		now := uint64(time.Now().UnixNano())
		for i := range b {
			b[i] = byte(now >> ((i % 8) * 8))
			now = now*1664525 + 1013904223
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b[:])
}

func uuidV5(namespace, name []byte) string {
	h := sha1.New()
	h.Write(namespace)
	h.Write(name)
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return formatUUID(sum)
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

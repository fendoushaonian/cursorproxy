package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalAuth 是从本机已登录的 Cursor 客户端读出的登录态。
// MachineID 优先用 storage.json 里的 telemetry.machineId（64 位），校验和认的是这个，不是 36 位的 service id。
type LocalAuth struct {
	AccessToken  string
	MachineID    string
	MacMachineID string
	Source       string
}

// ReadLocalAuth 读取 Cursor 状态库里的 access token 和 service machine id。
// path 为空时按当前系统的默认位置查找。环境变量仍可覆盖，这里只作缺省。
func ReadLocalAuth(path string) (LocalAuth, error) {
	if path == "" {
		found, err := defaultStateDB()
		if err != nil {
			return LocalAuth{}, err
		}
		path = found
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return LocalAuth{}, fmt.Errorf("读取 Cursor 登录态需要 sqlite3: %w", err)
	}
	query := `SELECT key, value FROM ItemTable WHERE key IN ('cursorAuth/accessToken','storage.serviceMachineId');`
	cmd := exec.Command("sqlite3", "-readonly", "-separator", "\t", "-noheader", path, query)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return LocalAuth{}, fmt.Errorf("读取 Cursor 状态库: %s", msg)
	}
	auth := LocalAuth{Source: path}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch key {
		case "cursorAuth/accessToken":
			auth.AccessToken = strings.TrimSpace(val)
		case "storage.serviceMachineId":
			auth.MachineID = strings.TrimSpace(val)
		}
	}
	if auth.AccessToken == "" {
		return LocalAuth{}, fmt.Errorf("Cursor 未登录（%s 里没有 access token）", path)
	}
	if machine, mac := readTelemetry(filepath.Dir(path)); machine != "" {
		auth.MachineID = machine
		auth.MacMachineID = mac
	}
	if auth.MachineID == "" {
		auth.MachineID = readMachineFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "machineid"))
	}
	return auth, nil
}

// readTelemetry 读和状态库同目录的 storage.json。校验和用的是这两项，不在 sqlite 里。
func readTelemetry(dir string) (machine, mac string) {
	b, err := os.ReadFile(filepath.Join(dir, "storage.json"))
	if err != nil {
		return "", ""
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return "", ""
	}
	machine, _ = doc["telemetry.machineId"].(string)
	mac, _ = doc["telemetry.macMachineId"].(string)
	return strings.TrimSpace(machine), strings.TrimSpace(mac)
}

func defaultStateDB() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	candidates := []string{
		filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"),
		filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb"),
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		candidates = append(candidates, filepath.Join(appdata, "Cursor", "User", "globalStorage", "state.vscdb"))
	}
	for _, p := range candidates {
		info, statErr := os.Stat(p)
		if statErr == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("没有找到本机 Cursor 状态库")
}

func readMachineFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

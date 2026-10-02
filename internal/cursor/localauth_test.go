package cursor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLocalAuthFromFixture(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.vscdb")
	sql := `CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO ItemTable(key, value) VALUES
('cursorAuth/accessToken', 'user_01::jwt-token'),
('storage.serviceMachineId', 'machine-from-db');`
	cmd := exec.Command("sqlite3", db, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v %s", err, out)
	}
	auth, err := ReadLocalAuth(db)
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != "user_01::jwt-token" || auth.MachineID != "machine-from-db" {
		t.Fatalf("token %q machine %q", auth.AccessToken, auth.MachineID)
	}
}

func TestReadLocalAuthLive(t *testing.T) {
	auth, err := ReadLocalAuth("")
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.AccessToken) < 20 || len(auth.MachineID) != 64 || len(auth.MacMachineID) != 64 {
		t.Fatalf("token len %d machine len %d mac len %d", len(auth.AccessToken), len(auth.MachineID), len(auth.MacMachineID))
	}
	if strings.Contains(auth.AccessToken, "\n") {
		t.Fatal("token contains newline")
	}
}

func TestReadLocalAuthMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadLocalAuth(filepath.Join(dir, "missing.vscdb")); err == nil {
		t.Fatal("expected error")
	}
	_ = os.Remove(filepath.Join(dir, "nope"))
}

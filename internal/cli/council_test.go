package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// councilEnv is Council's machine: its client config joined to a mesh as
// "council" through a kindless invite, and its own folder.
type councilEnv struct {
	mesh        *testrelay.Mesh
	config, dir string
}

func newCouncilEnv(t *testing.T) councilEnv {
	t.Helper()
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	m := testrelay.New(t, relay.Config{})
	m.JoinOnMachineOf(t, "muse", "council")
	e := councilEnv{mesh: m, config: filepath.Join(t.TempDir(), "council.json"), dir: filepath.Join(t.TempDir(), "tincan-council")}
	b, _ := json.Marshal(client.Config{Relay: m.URL("council"), Agent: "council"})
	if err := os.WriteFile(e.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e councilEnv) setKind(t *testing.T, kind string) {
	t.Helper()
	if err := e.mesh.Client(t, "admin").SetKind(t.Context(), "council", kind); err != nil {
		t.Fatal(err)
	}
}

// council serve refuses to run when the relay does not store kind council
// for it, saying why and how to fix it.
func TestCouncilServeRefusesWithoutCouncilKind(t *testing.T) {
	e := newCouncilEnv(t)
	_, _, err := runSplit(t, councilCmd(), "serve", "--config", e.config, "--dir", e.dir)
	if err == nil || !strings.Contains(err.Error(), "refuses to run") || !strings.Contains(err.Error(), "tincan kind council council") {
		t.Fatalf("serve = %v, want a refusal naming the kind fix", err)
	}
}

// doctor fails the relay kind check until the relay stores kind council,
// and passes once it does.
func TestCouncilDoctorChecksRelayKind(t *testing.T) {
	e := newCouncilEnv(t)
	out, _, err := runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err == nil {
		t.Fatalf("doctor passed with no kind:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay kind", "no kind", "tincan kind council council")

	e.setKind(t, "council")
	out, _, err = runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err != nil {
		t.Fatalf("doctor failed with kind council: %v\n%s", err, out)
	}
	wantLine(t, out, "[OK] relay kind", `"council"`)
	wantLine(t, out, "[OK] relay:")
	wantLine(t, out, "[OK] council.json", "using the defaults")
	wantLine(t, out, "[OK] data folder", e.dir)
	wantLine(t, out, "[OK] report folder", filepath.Join(e.dir, "reports"))
}

// A council.json that does not parse fails doctor.
func TestCouncilDoctorRejectsBadSettings(t *testing.T) {
	e := newCouncilEnv(t)
	e.setKind(t, "council")
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "council.json"), []byte(`{"memebers":["x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err == nil {
		t.Fatalf("doctor passed with a bad council.json:\n%s", out)
	}
	wantLine(t, out, "[FAIL] council.json", "memebers")
}

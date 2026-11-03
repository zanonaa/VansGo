package handlers

import (
	"context"
	"strings"
	"testing"
	"time"
)

// bootHarness monta o Headroom injetável para o boot: binário fake, spawn
// gravado em `spawns` e gravações de settings registradas em `writes`.
type bootHarness struct {
	routes *HeadroomRoutes
	spawns *[]string
	writes *[]map[string]any
}

func newBootHarness(t *testing.T) *bootHarness {
	t.Helper()
	spawns := &[]string{}
	writes := &[]map[string]any{}
	child := cmdWithPID(t)
	routes := &HeadroomRoutes{H: &Headroom{
		DataDir:     t.TempDir(),
		StartupWait: 150 * time.Millisecond,
		FindBinary:  func() string { return "/tmp/fake-headroom" },
		FindPython:  func() string { return "" },
		Spawn: func(binaryPath string, args []string, logFile string) (int, error) {
			*spawns = append(*spawns, strings.Join(args, " "))
			return child(), nil
		},
	}}
	return &bootHarness{routes: routes, spawns: spawns, writes: writes}
}

func (b *bootHarness) write(_ context.Context, updates map[string]any) (map[string]any, error) {
	merged := map[string]any{}
	for key, value := range updates {
		merged[key] = value
	}
	*b.writes = append(*b.writes, updates)
	return merged, nil
}

func TestBootHeadroomReconcileLegacyAndAutostart(t *testing.T) {
	b := newBootHarness(t)
	t.Setenv("HEADROOM_URL", "")
	b.routes.Read = func(context.Context) (map[string]any, error) {
		return map[string]any{"headroomEnabled": true, "headroomUrl": "http://headroom:8787"}, nil
	}
	if err := BootHeadroom(context.Background(), b.routes, b.write); err != nil {
		t.Fatal(err)
	}
	// URL do sidecar legado é reescrita para o default em loopback e o
	// proxy sobe no boot (pidfile/spawn registrados).
	if len(*b.writes) != 1 || (*b.writes)[0]["headroomUrl"] != "http://localhost:8787" {
		t.Fatalf("reconciliação %v", *b.writes)
	}
	if len(*b.spawns) != 1 || (*b.spawns)[0] != "proxy --port 8787 --disable-kompress" {
		t.Fatalf("spawn %v", *b.spawns)
	}
}

func TestBootHeadroomSkippedWhenDisabledOrExternal(t *testing.T) {
	b := newBootHarness(t)
	t.Setenv("HEADROOM_URL", "")
	// Toggle desativado: nenhum spawn, mas a URL legada aponta para um host
	// de serviço que não existirá mais no compose único e é reconciliada de
	// uma vez (migração idempotente; sem o write, religar o toggle depois
	// usaria EXTERNAL_PROXY contra um host morto).
	b.routes.Read = func(context.Context) (map[string]any, error) {
		return map[string]any{"headroomEnabled": false, "headroomUrl": "http://headroom:8787"}, nil
	}
	if err := BootHeadroom(context.Background(), b.routes, b.write); err != nil {
		t.Fatal(err)
	}
	if len(*b.spawns) != 0 || len(*b.writes) != 1 {
		t.Fatalf("disabled %v %v", *b.spawns, *b.writes)
	}
	// URL externa de verdade (host não legado): modo EXTERNAL_PROXY da
	// origem — sem spawn e sem nova gravação.
	b.routes.Read = func(context.Context) (map[string]any, error) {
		return map[string]any{"headroomEnabled": true, "headroomUrl": "https://headroom.remote.example"}, nil
	}
	if err := BootHeadroom(context.Background(), b.routes, b.write); err != nil {
		t.Fatal(err)
	}
	if len(*b.spawns) != 0 || len(*b.writes) != 1 {
		t.Fatalf("external %v %v", *b.spawns, *b.writes)
	}
	// URL legada do vansrouter também reconcilia e autostart ocorre.
	b.routes.Read = func(context.Context) (map[string]any, error) {
		return map[string]any{"headroomEnabled": true, "headroomUrl": "http://vansrouter:8787"}, nil
	}
	if err := BootHeadroom(context.Background(), b.routes, b.write); err != nil {
		t.Fatal(err)
	}
	if len(*b.spawns) != 1 || (*b.spawns)[0] != "proxy --port 8787 --disable-kompress" || len(*b.writes) != 2 {
		t.Fatalf("vansrouter %v %v", *b.spawns, *b.writes)
	}
}

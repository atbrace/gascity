package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// execScriptTestCity builds a city with an orders/ dir and a formulas/ layer,
// mirroring orderFiringTestCity but kept local so the two checks' fixtures
// stay independent.
func execScriptTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatalf("creating orders dir: %v", err)
	}
	return cityPath, &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{filepath.Join(cityPath, "formulas")},
		},
	}
}

func writeExecOrder(t *testing.T, cityPath, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityPath, "orders", name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing order %s: %v", name, err)
	}
}

func runExecScriptCheck(t *testing.T, cfg *config.City, cityPath string) *CheckResult {
	t.Helper()
	return NewOrderExecScriptsCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})
}

// A city-local order copied out of a pack keeps `$PACK_DIR/assets/scripts/x.sh`
// in its exec, but PACK_DIR now resolves to the CITY (the city-local formula
// layer's parent), where assets/scripts/ does not exist. Every fire exits 127.
// This is the shape that silently disabled orphan-sweep for 15 days.
func TestOrderExecScripts_PackScriptCopiedIntoCityOrders(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	writeExecOrder(t, cityPath, "orphan-sweep", `[order]
trigger = "cooldown"
interval = "15m"
exec = "$PACK_DIR/assets/scripts/orphan-sweep.sh"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "orphan-sweep") {
		t.Fatalf("details = %v, want the offending order named", result.Details)
	}
	if !strings.Contains(joined, filepath.Join(cityPath, "assets", "scripts", "orphan-sweep.sh")) {
		t.Fatalf("details = %v, want the resolved missing path so the operator sees WHERE PACK_DIR pointed", result.Details)
	}
}

// A manual-trigger order is exactly how a broken order gets parked, and
// order-firing-current only inspects cron/cooldown. The missing script must
// still be reported or the failure stays invisible.
func TestOrderExecScripts_ManualTriggerStillChecked(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	writeExecOrder(t, cityPath, "gate-sweep", `[order]
trigger = "manual"
interval = "10m"
exec = "$PACK_DIR/assets/scripts/gate-sweep.sh"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error for manual-trigger order; details = %v", result.Status, result.Details)
	}
}

func TestOrderExecScripts_PresentScriptPasses(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	scriptDir := filepath.Join(cityPath, "assets", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("creating script dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "gate-sweep.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	writeExecOrder(t, cityPath, "gate-sweep", `[order]
trigger = "cooldown"
interval = "10m"
exec = "$PACK_DIR/assets/scripts/gate-sweep.sh"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; details = %v", result.Status, result.Details)
	}
}

// Bare commands resolved from PATH, and execs whose first token still carries
// an unexpanded variable, are not statically resolvable — skip, never guess.
func TestOrderExecScripts_UnresolvableExecsSkipped(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	writeExecOrder(t, cityPath, "bare-command", `[order]
trigger = "cooldown"
interval = "10m"
exec = "gc bd gate check --escalate"
`)
	writeExecOrder(t, cityPath, "runtime-var", `[order]
trigger = "cooldown"
interval = "10m"
exec = "$SOME_RUNTIME_DIR/thing.sh"
`)
	writeExecOrder(t, cityPath, "inline-shell", `[order]
trigger = "cooldown"
interval = "10m"
exec = "sh -c 'echo hi'"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK for unresolvable execs; details = %v", result.Status, result.Details)
	}
}

// ORDER_DIR is the other path variable the dispatcher exports; a city-local
// order that references its own directory must resolve against the order file.
func TestOrderExecScripts_OrderDirResolves(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	writeExecOrder(t, cityPath, "local-helper", `[order]
trigger = "cooldown"
interval = "10m"
exec = "$ORDER_DIR/helper.sh"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; details = %v", result.Status, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), filepath.Join(cityPath, "orders", "helper.sh")) {
		t.Fatalf("details = %v, want ORDER_DIR resolved against the order file", result.Details)
	}
}

func TestOrderExecScripts_FormulaOrdersIgnored(t *testing.T) {
	cityPath, cfg := execScriptTestCity(t)
	writeExecOrder(t, cityPath, "docs-agent", `[order]
trigger = "cron"
schedule = "0 4 * * *"
formula = "mol-docs"
pool = "docs"
`)

	result := runExecScriptCheck(t, cfg, cityPath)
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; details = %v", result.Status, result.Details)
	}
}

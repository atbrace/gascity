package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

const orderExecScriptsName = "order-exec-scripts"

// OrderExecScriptsCheck verifies that every exec order whose command resolves
// to a static absolute path actually has that script on disk.
//
// A missing script makes the order exit 127 on every fire. Nothing else
// reports that: the dispatcher logs the failure and moves on, and
// order-firing-current only inspects cron and cooldown triggers — so parking a
// broken order at trigger = "manual" (the usual reflex) removes it from the
// only order-health surface there is. An order can therefore be dead for weeks
// while `gc order list` still shows it configured.
//
// The shape that produces this in practice: a pack-shipped order is COPIED
// into <city>/orders/ to adjust its cadence. The copy keeps
// `exec = "$PACK_DIR/assets/scripts/<name>.sh"`, but a city-local order's
// formula layer is <city>/formulas, so PACK_DIR now resolves to <city> — a
// directory with no assets/scripts. The order is silently detached from the
// pack that owns its script. Use [[orders.overrides]] to retune a pack order
// instead of copying it; this check names the ones already copied.
//
// Only statically resolvable execs are inspected. A first token that is a bare
// command (resolved from PATH), a relative path, or still carries an
// unexpanded variable after PACK_DIR / GC_PACK_DIR / ORDER_DIR substitution is
// skipped rather than guessed at.
type OrderExecScriptsCheck struct {
	cfg      *config.City
	cityPath string
}

// NewOrderExecScriptsCheck creates a check that validates exec order script
// references for every discovered city and rig order.
func NewOrderExecScriptsCheck(cfg *config.City, cityPath string) *OrderExecScriptsCheck {
	return &OrderExecScriptsCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderExecScriptsCheck) Name() string { return orderExecScriptsName }

// CanFix returns false — a missing script must be shipped with its pack or the
// order's exec corrected; doctor cannot invent either.
func (c *OrderExecScriptsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *OrderExecScriptsCheck) Fix(_ *CheckContext) error { return nil }

// Run reports every exec order whose resolved script path is missing on disk.
func (c *OrderExecScriptsCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	var issues []string
	checked := 0
	for _, order := range allOrders {
		if !order.IsExec() {
			continue
		}
		script, ok := resolveOrderExecScript(order)
		if !ok {
			continue
		}
		checked++
		if _, err := os.Stat(script); err == nil || !os.IsNotExist(err) {
			continue
		}
		issues = append(issues, fmt.Sprintf("%s: exec script %s not found (from %s)",
			orderDisplayName(order), script, order.Source))
	}

	if len(issues) == 0 {
		result.Status = StatusOK
		result.Message = fmt.Sprintf("all %d statically resolvable exec order script(s) exist", checked)
		return result
	}
	sort.Strings(issues)
	result.Status = StatusError
	// Advisory: a dead order is a real gap, but it does not make the city
	// unsafe to run, and gating dispatch on it would turn one stale order
	// into a city-wide stop.
	result.Severity = SeverityAdvisory
	result.Message = fmt.Sprintf("%d exec order(s) reference a script that does not exist — every fire exits 127", len(issues))
	result.FixHint = "ship the script with its pack, or fix the order's exec. If the order was copied out of a pack into <city>/orders/ to retune it, delete the copy and use [[orders.overrides]] instead — a city-local copy repoints $PACK_DIR at the city."
	result.Details = issues
	return result
}

// resolveOrderExecScript returns the absolute script path an exec order will
// invoke, when that path is statically determinable. It substitutes the
// path-valued environment variables the dispatcher exports (see
// orderExecEnvWithError): PACK_DIR / GC_PACK_DIR from the order's formula
// layer, and ORDER_DIR from the order file's directory.
func resolveOrderExecScript(order orders.Order) (string, bool) {
	expanded := order.Exec
	if order.FormulaLayer != "" {
		packDir := filepath.Dir(order.FormulaLayer)
		expanded = replaceShellVar(expanded, "PACK_DIR", packDir)
		expanded = replaceShellVar(expanded, "GC_PACK_DIR", packDir)
	}
	if order.Source != "" {
		expanded = replaceShellVar(expanded, "ORDER_DIR", filepath.Dir(order.Source))
	}

	fields := strings.Fields(expanded)
	if len(fields) == 0 {
		return "", false
	}
	first := fields[0]
	// Anything still carrying a variable or template needs runtime context.
	if strings.ContainsAny(first, "$`") || strings.Contains(first, "{{") {
		return "", false
	}
	if !filepath.IsAbs(first) {
		return "", false
	}
	return filepath.Clean(first), true
}

// replaceShellVar substitutes both ${NAME} and $NAME forms. The ${NAME} form
// is replaced first so $NAME does not eat the brace form's leading token.
func replaceShellVar(s, name, value string) string {
	s = strings.ReplaceAll(s, "${"+name+"}", value)
	return strings.ReplaceAll(s, "$"+name, value)
}

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var legacySessionProviderFactoryReferences = map[string]int{
	"cmd_convoy_dispatch.go:<package>:newSessionProvider":                                   1,
	"cmd_doctor.go:buildDoctorChecks:newSessionProvider":                                    1,
	"cmd_handoff.go:cmdHandoff:newSessionProvider":                                          1,
	"cmd_handoff.go:cmdHandoffRemote:newSessionProvider":                                    1,
	"cmd_nudge.go:cmdNudgePoll:newSessionProvider":                                          1,
	"cmd_nudge.go:deliverSessionNudge:newSessionProvider":                                   1,
	"cmd_nudge.go:sendMailNotify:newSessionProvider":                                        1,
	"cmd_rig.go:<package>:newSessionProvider":                                               1,
	"cmd_sling.go:cmdSlingWithJSON:newSessionProvider":                                      1,
	"session_template_start.go:materializeSessionForAgentConfig:newSessionProvider":         1,
	"session_template_start.go:materializeSessionForTemplateWithOptions:newSessionProvider": 1,
}

var legacySessionProviderAliasCalls = map[string]int{
	"cmd_convoy_dispatch.go:runControlDispatcherWithStoreAndConfig:dispatchControlSessionProvider": 2,
	"cmd_rig.go:doRigList:rigListSessionProvider":                                                  1,
}

var legacySessionProviderAliasBindings = map[string]int{
	"cmd_convoy_dispatch.go:<package>:dispatchControlSessionProvider=newSessionProvider": 1,
	"cmd_rig.go:<package>:rigListSessionProvider=newSessionProvider":                     1,
}

var legacySessionProviderExitHelperUses = map[string]int{
	"providers.go:newSessionProvider:sessionProviderOrExit":                          1,
	"providers.go:newSessionProviderForCity:sessionProviderOrExit":                   1,
	"providers.go:newSessionProviderFromContext:sessionProviderOrExit":               1,
	"providers.go:newStatusSessionProviderForCity:sessionProviderOrExit":             1,
	"providers.go:newStatusSessionProviderForCityWithSnapshot:sessionProviderOrExit": 1,
}

var legacySessionProviderFactories = map[string]bool{
	"newSessionProvider":                          true,
	"newSessionProviderForCity":                   true,
	"newSessionProviderFromContext":               true,
	"newStatusSessionProviderForCity":             true,
	"newStatusSessionProviderForCityWithSnapshot": true,
}

var legacySessionProviderExitHelperOwners = map[string]bool{
	"newSessionProvider":                          true,
	"newSessionProviderForCity":                   true,
	"newSessionProviderFromContext":               true,
	"newStatusSessionProviderForCity":             true,
	"newStatusSessionProviderForCityWithSnapshot": true,
}

type providerFactoryCensus struct {
	references     map[string]int
	aliasBindings  map[string]int
	aliasCalls     map[string]int
	aliases        map[string]bool
	exitHelperUses map[string]int
	directCalls    int
	violations     []string
}

func (c providerFactoryCensus) invocationCount() int {
	invocations := c.directCalls
	for _, count := range c.aliasCalls {
		invocations += count
	}
	return invocations
}

func TestLegacySessionProviderFactoryCallerCensus(t *testing.T) {
	dir, err := providerFactorySourceDir()
	if err != nil {
		t.Fatal(err)
	}
	census, err := scanLegacySessionProviderFactoryCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(census.violations) != 0 {
		t.Fatalf("legacy provider factory census found unclassified uses:\n%s", strings.Join(census.violations, "\n"))
	}
	if !maps.Equal(census.references, legacySessionProviderFactoryReferences) {
		t.Fatalf("legacy provider factory reference census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.references), formatProviderFactoryCensus(legacySessionProviderFactoryReferences))
	}
	if !maps.Equal(census.aliasBindings, legacySessionProviderAliasBindings) {
		t.Fatalf("legacy provider factory alias-binding census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.aliasBindings), formatProviderFactoryCensus(legacySessionProviderAliasBindings))
	}
	if !maps.Equal(census.aliasCalls, legacySessionProviderAliasCalls) {
		t.Fatalf("legacy provider factory alias-call census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.aliasCalls), formatProviderFactoryCensus(legacySessionProviderAliasCalls))
	}
	if !maps.Equal(census.exitHelperUses, legacySessionProviderExitHelperUses) {
		t.Fatalf("sessionProviderOrExit ownership census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.exitHelperUses), formatProviderFactoryCensus(legacySessionProviderExitHelperUses))
	}
	if census.directCalls != 9 {
		t.Fatalf("direct legacy provider factory invocation count = %d, want 9", census.directCalls)
	}
	if invocations := census.invocationCount(); invocations != 12 {
		t.Fatalf("legacy provider factory invocation count = %d, want 12", invocations)
	}
}

func TestProviderFactoryCensusExcludesDeclarationsButScansProvidersGo(t *testing.T) {
	census := scanProviderFactoryFixture(t, "providers.go", `package main

func newSessionProvider() {}
func newSessionProviderForCity() {}
func newSessionProviderFromContext() {}
func newStatusSessionProviderForCity() {}
func newStatusSessionProviderForCityWithSnapshot() {}
func sessionProviderOrExit() {}
`)
	if len(census.references) != 0 || len(census.aliasBindings) != 0 || len(census.aliasCalls) != 0 || len(census.exitHelperUses) != 0 || census.directCalls != 0 || len(census.violations) != 0 {
		t.Fatalf("declarations were counted as uses: %#v", census)
	}
}

func TestProviderFactoryCensusTracksSecondOrderPackageAliasesToFixedPoint(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

var first = newSessionProvider
var second = first

func invoke() { second() }
`)
	for _, alias := range []string{"first", "second"} {
		if !census.aliases[alias] {
			t.Fatalf("alias %q was not tracked: %#v", alias, census.aliases)
		}
	}
	if got := census.aliasBindings["fixture.go:<package>:second=first"]; got != 1 {
		t.Fatalf("second-order alias binding count = %d, want 1; census=%s", got, formatProviderFactoryCensus(census.aliasBindings))
	}
	if got := census.aliasCalls["fixture.go:invoke:second"]; got != 1 {
		t.Fatalf("second-order alias invocation count = %d, want 1; census=%s", got, formatProviderFactoryCensus(census.aliasCalls))
	}
	if got := census.invocationCount(); got != 1 {
		t.Fatalf("fixture invocation count = %d, want 1", got)
	}
}

func TestProviderFactoryCensusRejectsCallbackEscape(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func acceptProviderFactory(any) {}
func evade() { acceptProviderFactory(newSessionProvider) }
`)
	want := "fixture.go:evade:newSessionProvider is a non-call provider factory use"
	if !slices.Contains(census.violations, want) {
		t.Fatalf("callback escape violations = %q, want %q", census.violations, want)
	}
}

func TestProviderFactoryCensusRejectsDirectExitHelperOutsideCompatibilityWrappers(t *testing.T) {
	census := scanProviderFactoryFixture(t, "providers.go", `package main

func evade() { sessionProviderOrExit(nil, nil) }
`)
	want := "providers.go:evade:sessionProviderOrExit is outside the five compatibility wrappers"
	if !slices.Contains(census.violations, want) {
		t.Fatalf("direct exit-helper violations = %q, want %q", census.violations, want)
	}
}

func scanProviderFactoryFixture(t *testing.T, name, source string) providerFactoryCensus {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
	census, err := scanLegacySessionProviderFactoryCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	return census
}

func providerFactorySourceDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get provider factory census working directory: %w", err)
	}
	for _, candidate := range []string{cwd, filepath.Join(cwd, "cmd", "gc")} {
		info, statErr := os.Stat(filepath.Join(candidate, "providers.go"))
		if statErr == nil && !info.IsDir() {
			return filepath.Clean(candidate), nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect provider factory source directory %q: %w", candidate, statErr)
		}
	}
	return "", fmt.Errorf("locate cmd/gc provider sources from working directory %q", cwd)
}

type parsedProviderFactoryFile struct {
	name string
	file *ast.File
}

type providerAliasBinding struct {
	left  string
	right string
}

func scanLegacySessionProviderFactoryCallers(dir string) (providerFactoryCensus, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return providerFactoryCensus{}, fmt.Errorf("read provider factory source directory %q: %w", dir, err)
	}

	var files []parsedProviderFactoryFile
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return providerFactoryCensus{}, fmt.Errorf("parse provider factory source %q: %w", name, err)
		}
		files = append(files, parsedProviderFactoryFile{name: name, file: parsed})
	}

	aliases := discoverProviderFactoryAliases(files)
	bindingUses, aliasBindings := providerFactoryAliasBindings(files, aliases)
	census := providerFactoryCensus{
		references:     map[string]int{},
		aliasBindings:  aliasBindings,
		aliasCalls:     map[string]int{},
		aliases:        aliases,
		exitHelperUses: map[string]int{},
	}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			functionName, roots := providerDeclarationRoots(declaration)
			for _, root := range roots {
				scanProviderFactoryDeclaration(parsed.name, functionName, root, bindingUses, &census)
			}
		}
	}
	slices.Sort(census.violations)
	return census, nil
}

func discoverProviderFactoryAliases(files []parsedProviderFactoryFile) map[string]bool {
	aliases := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, parsed := range files {
			for _, declaration := range parsed.file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.VAR {
					continue
				}
				for _, spec := range general.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, value := range values.Values {
						if index >= len(values.Names) {
							break
						}
						identifier, ok := value.(*ast.Ident)
						if !ok || (!legacySessionProviderFactories[identifier.Name] && !aliases[identifier.Name]) {
							continue
						}
						alias := values.Names[index].Name
						if alias == "_" || legacySessionProviderFactories[alias] || aliases[alias] {
							continue
						}
						aliases[alias] = true
						changed = true
					}
				}
			}
		}
	}
	return aliases
}

func providerFactoryAliasBindings(files []parsedProviderFactoryFile, aliases map[string]bool) (map[*ast.Ident]providerAliasBinding, map[string]int) {
	bindingUses := map[*ast.Ident]providerAliasBinding{}
	bindings := map[string]int{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, value := range values.Values {
					if index >= len(values.Names) {
						break
					}
					identifier, ok := value.(*ast.Ident)
					if !ok || (!legacySessionProviderFactories[identifier.Name] && !aliases[identifier.Name]) {
						continue
					}
					binding := providerAliasBinding{left: values.Names[index].Name, right: identifier.Name}
					bindingUses[identifier] = binding
					bindings[fmt.Sprintf("%s:<package>:%s=%s", parsed.name, binding.left, binding.right)]++
				}
			}
		}
	}
	return bindingUses, bindings
}

func providerDeclarationRoots(declaration ast.Decl) (string, []ast.Node) {
	switch typed := declaration.(type) {
	case *ast.FuncDecl:
		if typed.Body == nil {
			return typed.Name.Name, nil
		}
		return typed.Name.Name, []ast.Node{typed.Body}
	case *ast.GenDecl:
		roots := make([]ast.Node, 0, len(typed.Specs))
		for _, spec := range typed.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, value := range values.Values {
				roots = append(roots, value)
			}
		}
		return "<package>", roots
	default:
		return "<package>", nil
	}
}

func scanProviderFactoryDeclaration(fileName, functionName string, root ast.Node, bindingUses map[*ast.Ident]providerAliasBinding, census *providerFactoryCensus) {
	directCallUses := map[*ast.Ident]bool{}
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if identifier, ok := call.Fun.(*ast.Ident); ok {
			directCallUses[identifier] = true
		}
		return true
	})

	ast.Inspect(root, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		key := fmt.Sprintf("%s:%s:%s", fileName, functionName, identifier.Name)
		isDirectCall := directCallUses[identifier]
		_, isBinding := bindingUses[identifier]

		if legacySessionProviderFactories[identifier.Name] {
			census.references[key]++
			if isDirectCall {
				census.directCalls++
			} else if !isBinding {
				census.violations = append(census.violations, key+" is a non-call provider factory use")
			}
			return true
		}
		if census.aliases[identifier.Name] {
			if isDirectCall {
				census.aliasCalls[key]++
			} else if !isBinding {
				census.violations = append(census.violations, key+" is a non-call provider factory use")
			}
			return true
		}
		if identifier.Name == "sessionProviderOrExit" {
			census.exitHelperUses[key]++
			if !isDirectCall {
				census.violations = append(census.violations, key+" is a non-call exit-helper use")
			} else if fileName != "providers.go" || !legacySessionProviderExitHelperOwners[functionName] {
				census.violations = append(census.violations, key+" is outside the five compatibility wrappers")
			}
		}
		return true
	})
}

func formatProviderFactoryCensus(census map[string]int) string {
	entries := make([]string, 0, len(census))
	for caller, count := range census {
		entries = append(entries, fmt.Sprintf("%s = %d", caller, count))
	}
	slices.Sort(entries)
	return strings.Join(entries, "\n")
}

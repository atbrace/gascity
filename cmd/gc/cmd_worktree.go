package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// hqWorktreeSetupScriptRelPath is the worktree-setup.sh path, relative to
// the city root. HQ worktrees are provisioned by the same unmodified script
// rig worktrees use, invoked against the city repo itself instead of a rig.
const hqWorktreeSetupScriptRelPath = "packs/gastown/scripts/worktree-setup.sh"

func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage per-bead git worktrees",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc worktree: missing subcommand (hq)") //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0]) //nolint:errcheck
			}
			return errExit
		},
	}
	cmd.AddCommand(newWorktreeHQCmd(stdout, stderr))
	return cmd
}

func newWorktreeHQCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "hq <bead-id>",
		Short: "Provision (or reuse) a dedicated worktree for HQ-targeting bead work",
		Long: `Provision an isolated git worktree of the city repo itself, for bead work
that targets HQ (the city) rather than a rig.

Idempotent: reuses an existing worktree for the calling role and bead ID if
one already exists; otherwise creates one via worktree-setup.sh. Always
freshens onto the latest main via rebase (--freshen-commit), never resets
it. The worktree's .beads/redirect is unconditionally rewritten to point at
the city's own beads store.

The calling role is resolved from $GC_TEMPLATE (preferred) or $GC_AGENT.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree hq: %v\n", err) //nolint:errcheck
				return errExit
			}
			path, err := doWorktreeHQ(cityPath, args[0], stdout, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree hq: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintln(stdout, path) //nolint:errcheck
			return nil
		},
	}
}

// resolveCallingRole resolves the bare (non-rig-qualified) role name of the
// calling agent from $GC_TEMPLATE (preferred) or $GC_AGENT, e.g.
// "gascity/builder" -> "builder". Returns "" if neither is set.
func resolveCallingRole() string {
	identity := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
	if identity == "" {
		identity = strings.TrimSpace(os.Getenv("GC_AGENT"))
	}
	if identity == "" {
		return ""
	}
	_, name := config.ParseQualifiedName(identity)
	return name
}

// doWorktreeHQ provisions (or reuses) an HQ-targeting bead worktree at
// cityPath/.gc/worktrees/_hq/<role>-<beadID> via worktree-setup.sh, always
// with --freshen-commit, and unconditionally rewrites the worktree's
// .beads/redirect to point at cityPath/.beads. Returns the worktree's
// absolute path.
func doWorktreeHQ(cityPath, beadID string, stdout, stderr io.Writer) (string, error) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return "", fmt.Errorf("missing bead ID")
	}
	role := resolveCallingRole()
	if role == "" {
		return "", fmt.Errorf("cannot resolve calling role: neither GC_TEMPLATE nor GC_AGENT is set")
	}

	worktreeDir := filepath.Join(cityPath, ".gc", "worktrees", hqBeadWorktreeBucket, role+"-"+beadID)
	scriptPath := filepath.Join(cityPath, hqWorktreeSetupScriptRelPath)

	cmd := exec.Command(scriptPath, cityPath, worktreeDir, role, "--freshen-commit") // #nosec G204 -- fixed, unmodified in-repo script
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("worktree-setup.sh: %w", err)
	}

	if err := writeHQBeadsRedirect(worktreeDir, cityPath); err != nil {
		return "", err
	}
	return worktreeDir, nil
}

// writeHQBeadsRedirect unconditionally (re)writes worktreeDir/.beads/redirect
// to point at cityPath/.beads, so bd commands run from inside the worktree
// resolve to the city's own beads store.
func writeHQBeadsRedirect(worktreeDir, cityPath string) error {
	beadsDir := filepath.Join(worktreeDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", beadsDir, err)
	}
	target := filepath.Join(cityPath, ".beads") + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(target), 0o644); err != nil {
		return fmt.Errorf("writing .beads/redirect: %w", err)
	}
	return nil
}

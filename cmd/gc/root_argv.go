package main

import "strings"

// rootCommandOptions controls side effects performed while constructing the
// Cobra tree. invocationArgs is always the injected run(args) slice and never
// includes argv[0].
type rootCommandOptions struct {
	invocationArgs            []string
	discoverPackCommands      bool
	eagerPackCommandDiscovery bool
}

func rootCommandOptionsForArgs(args []string) rootCommandOptions {
	command, ok := firstRootCommand(args)
	discoverPackCommands := !ok || command != "metrics"
	return rootCommandOptions{
		invocationArgs:            append([]string(nil), args...),
		discoverPackCommands:      discoverPackCommands,
		eagerPackCommandDiscovery: discoverPackCommands,
	}
}

// firstRootCommand returns the first command word under the root's narrow
// persistent-scope grammar. Unknown flags fail closed because this pre-scan
// cannot know whether a later token is their value. A separate --city/--rig
// form consumes exactly one following token, including "--", matching pflag.
func firstRootCommand(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return "", false
		case arg == "--city" || arg == "--rig":
			if index+1 >= len(args) {
				return "", false
			}
			index++
		case strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig="):
			continue
		case strings.HasPrefix(arg, "-"):
			return "", false
		default:
			return arg, true
		}
	}
	return "", false
}

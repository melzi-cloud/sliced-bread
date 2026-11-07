// Command sb is the SlicedBread CLI: installing (sb init) and operating apps. Developer
// tooling (test installs, the reaper, smoke tests) is a separate binary in hack/sb-dev.
package main

import (
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "init":
			os.Exit(handleExit(runInit(args[1:])))
		case "destroy":
			os.Exit(handleExit(runDestroy(args[1:])))
		case "installs":
			os.Exit(handleExit(runInstalls()))
		case "upgrade":
			os.Exit(handleExit(runUpgrade(args[1:])))
		case "ssh":
			os.Exit(handleExit(runSSH(args[1:])))
		case "setup-link":
			os.Exit(handleExit(runSetupLink(args[1:])))
		}
	}
	os.Exit(handleExit(runUser(args)))
}

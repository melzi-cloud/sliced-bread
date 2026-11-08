// Command sb is the SlicedBread CLI: installing (sb init) and operating apps. Developer
// tooling (test installs, the reaper, smoke tests) is a separate binary in hack/sb-dev.
package main

import (
	"fmt"
	"os"

	"github.com/melzi-cloud/sliced-bread/internal/version"
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
		case "version", "--version", "-v":
			fmt.Println("sb", version.Get())
			return
		}
	}
	os.Exit(handleExit(runUser(args)))
}

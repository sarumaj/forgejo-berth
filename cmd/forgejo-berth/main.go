// SPDX-License-Identifier: GPL-3.0-or-later

// Command forgejo-berth gives each waiting Forgejo Actions job an ephemeral runner on a
// container slot of a fixed host where its learned memory peak fits.
package main

import (
	"fmt"
	"os"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3").
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: forgejo-berth version")
	os.Exit(2)
}

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/audiocodes/terraform-provider-scripted/internal/provider"
)

// version is set by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

// pythonHelper is the vendorable helper library for programs written in
// Python; `--emit-helper` prints it so users get the copy matching this
// provider.
//
//go:embed helpers/python/scripted.py
var pythonHelper string

func main() {
	var debug, emitHelper bool

	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.BoolVar(&emitHelper, "emit-helper", false, "print the Python helper library (scripted.py) and exit")
	flag.Parse()

	if emitHelper {
		// Stamp the file with the version it came from.
		fmt.Fprint(os.Stdout, strings.Replace(pythonHelper, `HELPER_VERSION = "dev"`, fmt.Sprintf("HELPER_VERSION = %q", version), 1))
		return
	}

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/audiocodes/scripted",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}

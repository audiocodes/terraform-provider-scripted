// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/audiocodes/terraform-provider-scripted/helpers"
	"github.com/audiocodes/terraform-provider-scripted/internal/provider"
)

// version is set by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var debug, emitPython bool

	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.BoolVar(&emitPython, "emit-python-helper", false,
		"print the Python helper library (scripted.py) of this provider version and exit")
	flag.Parse()

	if emitPython {
		fmt.Fprint(os.Stdout, helpers.Python(version))
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

// Command terraform-provider-pveguest serves the pveguest OpenTofu provider
// over the plugin protocol.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const providerAddress = "tofu.home.arpa/agoodkind/pveguest"

// The install-mirror target sets this value at link time.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers")
	flag.Parse()

	options := providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	}
	slog.Info("provider server starting", "address", providerAddress, "version", version, "debug", debug)
	if err := providerserver.Serve(context.Background(), provider.New(version), options); err != nil {
		slog.Error("provider server stopped", "address", providerAddress, "err", err)
		os.Exit(1)
	}
}

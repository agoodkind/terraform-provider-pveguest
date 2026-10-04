package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const providerAddress = "tofu.home.arpa/agoodkind/pveguest"

// The Makefile overrides this value at link time.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers")
	flag.Parse()

	options := providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	}
	if err := providerserver.Serve(context.Background(), provider.New(version), options); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"github.com/protochron/nomad-driver-wasm/wasm"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"
)

func main() {
	// Serve the plugin
	plugins.Serve(factory)
}

func factory(log log.Logger) interface{} {
	return wasm.NewWasmTask(log)
}

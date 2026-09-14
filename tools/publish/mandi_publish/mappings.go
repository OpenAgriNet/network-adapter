package main

import "embed"

// mappingFiles carries this tool's mappings into the binary, so a run needs no
// working directory and a released tool cannot be pointed at an edited mapping
// by accident.
//
//go:embed mappings
var mappingFiles embed.FS

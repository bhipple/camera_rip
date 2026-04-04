//go:build !testbuild

package main

import "embed"

//go:embed all:frontend/build
var frontend embed.FS

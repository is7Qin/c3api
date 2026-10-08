// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Command pgtestctl is the database-side helper behind scripts/test.sh. It
// picks the default test parallelism, fails fast when the test role cannot
// create databases, and sweeps template databases left by older schema sources.
//
// It lives in the test-support tree and is run with `go run .../pgtestctl`.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "p":
		fmt.Println(defaultParallelism())
	case "precheck":
		if err := pgtest.Precheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "sweep":
		dropped, err := pgtest.SweepStaleTemplates(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, name := range dropped {
			fmt.Println("swept stale template:", name)
		}
	default:
		usage()
	}
}

// defaultParallelism caps concurrent test binaries at min(GOMAXPROCS, 8), so a
// machine with many cores does not open hundreds of database clones at once.
func defaultParallelism() int {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		return 8
	}
	if n < 1 {
		return 1
	}
	return n
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pgtestctl <p|precheck|sweep>")
	os.Exit(2)
}

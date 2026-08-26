// Generates CBOR marshal/unmarshal code for curetr.BulkRequest.
// Run from repo root: go run ./scripts/cborgen-curetr
package main

import (
	"fmt"
	"os"
	"path/filepath"

	cborgen "github.com/whyrusleeping/cbor-gen"

	"github.com/filecoin-project/curio/lib/curetr"
)

func main() {
	genName := filepath.Join("lib", "curetr", "bulk_cbor_gen.go")

	fmt.Print("Generating Cbor Marshal/Unmarshal for curetr.BulkRequest...")
	if err := cborgen.WriteMapEncodersToFile(genName, "curetr", curetr.BulkRequest{}); err != nil {
		fmt.Println("Failed:")
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("Done.")
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
)

// mountedElsewhere names the operations that are deliberately not mounted
// through the generated wrapper, with the reason.
//
// StreamEvents parses its own Last-Event-ID and is mounted by hand: an
// EventSource retries a rejected request forever with the same header, so a
// resume point the wrapper cannot parse must degrade to a fresh stream rather
// than become a reconnect loop. It is still routed — see the /events line in
// Handler.
var mountedElsewhere = []string{"StreamEvents"}

// Every operation the contract declares is actually routed.
//
// The compile lock in api_server.go proves the Server has a *method* for each
// operation. It does not prove anything calls it. GetCallTranscript had a
// method, a handler file, unit-level coverage of its authorization rule and a
// published contract entry, and no route — so in the running product the
// transcript snapshot returned the SPA's index.html with a 200, the panel's
// fetch failed to parse it, and the whole backfill half of the feature was
// dead. Nothing failed. This is what would have failed.
func TestEveryContractOperationIsRouted(t *testing.T) {
	iface := reflect.TypeOf((*api.ServerInterface)(nil)).Elem()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	// Collect every op.X referenced in the router, plus every method called
	// directly on the server, which is how the hand-mounted ones appear.
	routed := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && (ident.Name == "op" || ident.Name == "s") {
			routed[sel.Sel.Name] = true
		}
		return true
	})

	var missing []string
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		if routed[name] || slices.Contains(mountedElsewhere, name) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("declared in the contract and never routed: %s\n"+
			"A handler with no route is a 404 that answers 200 with the SPA, "+
			"which no client can tell from a broken response.",
			strings.Join(missing, ", "))
	}
}

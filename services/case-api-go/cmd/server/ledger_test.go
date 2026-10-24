// Author: Patrick Munis at newwave
package main

import (
	"testing"

	tb_types "github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// Reproduces the live finding (sibling deployment, main branch, same code):
// before this fix, a PENDING transfer and its later POST/VOID reused the
// exact same TigerBeetle transfer id (and PendingID was never set), which
// TigerBeetle correctly rejects as an id collision -- every two-phase
// transfer got stuck pending forever. The fix must produce a DIFFERENT id
// for POST/VOID than PENDING used, and that id's PendingID must equal the
// PENDING call's own id.
func TestTransferIDsPostHasDistinctIDReferencingPending(t *testing.T) {
	pendingID, zero := transferIDs("case-1", "ADMIN_FEE", "p1", 5000, "PENDING")
	if zero != (tb_types.Uint128{}) {
		t.Fatalf("a PENDING transfer must not set PendingID, got %v", zero)
	}

	postID, postPendingID := transferIDs("case-1", "ADMIN_FEE", "p1", 5000, "POST")
	if postID == pendingID {
		t.Fatal("POST must get its own id, distinct from the PENDING transfer's id -- " +
			"reusing it is exactly the id collision TigerBeetle rejected live")
	}
	if postPendingID != pendingID {
		t.Fatalf("POST's PendingID must equal the original PENDING transfer's id: got %v, want %v",
			postPendingID, pendingID)
	}

	voidID, voidPendingID := transferIDs("case-1", "ADMIN_FEE", "p1", 5000, "VOID")
	if voidID == pendingID || voidID == postID {
		t.Fatal("VOID must also get its own id, distinct from both PENDING and POST")
	}
	if voidPendingID != pendingID {
		t.Fatal("VOID's PendingID must also reference the original PENDING transfer's id")
	}
}

func TestTransferIDsDeterministic(t *testing.T) {
	a1, _ := transferIDs("case-1", "ADMIN_FEE", "p1", 5000, "PENDING")
	a2, _ := transferIDs("case-1", "ADMIN_FEE", "p1", 5000, "PENDING")
	if a1 != a2 {
		t.Fatal("the same request must hash to the same id every time -- " +
			"it's the API's own idempotency mechanism for duplicate client retries")
	}
}

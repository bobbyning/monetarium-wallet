// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"

	"github.com/monetarium/monetarium-wallet/wallet/udb"
)

// TestSyncLastReturnedAddressMovesRequestedBranch verifies that
// SyncLastReturnedAddress advances the cursor of the branch it is asked
// to sync and leaves the other branch alone. It is a regression test for
// the external branch case picking up the internal address buffer, so
// accountsyncaddressindex <account> 0 <index> moved the change-address
// cursor instead of the external one.
func TestSyncLastReturnedAddressMovesRequestedBranch(t *testing.T) {
	ctx := context.Background()
	cfg := basicWalletConfig
	w, teardown := testWallet(ctx, t, &cfg, nil)
	defer teardown()

	w.SetNetworkBackend(mockNetwork{})

	// The cursor of a branch only moves when the synced child is beyond
	// the branch's last used child. Give both branches a last used child
	// so the test observes exactly which buffer is advanced.
	w.addressBuffersMu.Lock()
	w.addressBuffers[0].albExternal.lastUsed = 10
	w.addressBuffers[0].albInternal.lastUsed = 5
	w.addressBuffersMu.Unlock()

	cursors := func() (ext, internal uint32) {
		w.addressBuffersMu.Lock()
		defer w.addressBuffersMu.Unlock()
		b := w.addressBuffers[0]
		return b.albExternal.cursor, b.albInternal.cursor
	}

	err := w.SyncLastReturnedAddress(ctx, 0, udb.ExternalBranch, 30)
	if err != nil {
		t.Fatal(err)
	}

	ext, internal := cursors()
	if ext != 20 {
		t.Fatalf("external cursor: got %d, want 20 (child 30 past last used 10)", ext)
	}
	if internal != 0 {
		t.Fatalf("internal cursor: got %d, want 0 (the internal branch was not synced)",
			internal)
	}

	// The same call for the internal branch must move the internal
	// cursor and leave the external one alone.
	err = w.SyncLastReturnedAddress(ctx, 0, udb.InternalBranch, 25)
	if err != nil {
		t.Fatal(err)
	}

	ext, internal = cursors()
	if internal != 20 {
		t.Fatalf("internal cursor: got %d, want 20 (child 25 past last used 5)", internal)
	}
	if ext != 20 {
		t.Fatalf("external cursor: got %d, want 20 (unchanged by the internal sync)", ext)
	}
}

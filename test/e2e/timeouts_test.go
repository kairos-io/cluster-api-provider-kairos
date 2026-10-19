package e2e

import "testing"

// The live-media fast-fail only ever runs if its grace period elapses inside the
// control-plane wait. Nothing at compile time ties the two together, so lowering
// cpTimeout below liveMediaStallGrace would turn detectLiveMediaStall into dead
// code and put the e2e silently back on the full timeout, with the misleading
// WaitingForNodePush verdict it produces. Assert the ordering instead of trusting
// the two constants to be edited together.
//
// This runs in the normal `go test ./... -short` job: TestE2E skips there, but a
// plain test in this package still runs, so the invariant is checked on every PR
// rather than only when someone spends an hour on the e2e.
func TestLiveMediaStallGraceLeavesRoomInsideTheControlPlaneTimeout(t *testing.T) {
	if liveMediaStallGrace >= cpTimeout {
		t.Fatalf("liveMediaStallGrace (%s) must be shorter than cpTimeout (%s), or the guest-stall fast-fail can never fire", liveMediaStallGrace, cpTimeout)
	}
	// It also has to leave enough of the budget behind to be worth having. If the
	// grace crept up to within a few minutes of the timeout, the fast-fail would
	// still "work" while saving nothing, which is the failure mode that looks fine
	// in review.
	if saved := cpTimeout - liveMediaStallGrace; saved < cpTimeout/2 {
		t.Errorf("a stalled guest would only save %s of the %s budget; the grace period (%s) has grown too close to the timeout to be useful", saved, cpTimeout, liveMediaStallGrace)
	}
}

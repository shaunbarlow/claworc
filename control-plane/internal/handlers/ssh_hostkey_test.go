package handlers

import "testing"

// clearInstanceHostKey runs on every container-replacing operation (image
// update, restart, resource and placement changes), including on the image
// update failure path. SSHMgr is nil in tests and before main.go finishes
// wiring, so a nil manager must be a safe no-op rather than a panic that would
// take down the background task mid-update.
//
// The clearing semantics themselves -- that dropping the pin lets a new host
// key be accepted after a legitimate restart -- are covered by
// sshproxy.TestSecurity_ClearHostKeyAllowsNewKey, and the MITM rejection that
// makes the explicit clear necessary by
// sshproxy.TestSecurity_TOFURejectsChangedHostKey. Asserting them again here
// would mean exporting SSHManager's private hostKeys map purely for a test.
func TestClearInstanceHostKey_NilManagerIsSafe(t *testing.T) {
	orig := SSHMgr
	t.Cleanup(func() { SSHMgr = orig })
	SSHMgr = nil
	clearInstanceHostKey(7, "unit test") // must not panic
}

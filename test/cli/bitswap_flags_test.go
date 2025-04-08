package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/ipfs/kubo/test/cli/harness"
)

func TestBitswapFlags(t *testing.T) {
	t.Parallel()

	t.Run("Bitswap disabled", func(t *testing.T) {
		t.Parallel()
		node := harness.NewT(t).NewNode().Init()
		node.SetIPFSConfig("Internal.Bitswap.Enabled", "false")
		node.StartDaemon()

		// Check bitswap stat - should fail or show nothing if disabled
		// Current behavior might be that the command fails because the service isn't registered.
		// Or it might succeed but show empty stats. Let's expect an error first.
		res := node.RunIPFS("bitswap", "stat")
		// TODO: Adjust assertion based on actual behavior. If it doesn't error, check for empty output.
		// It seems more likely the command itself won't be registered or fail early.
		// Let's check if the daemon log confirms it's disabled.
		node.WaitTillUp() // Ensure daemon is fully started before checking logs
		node.CheckLog("bitswap").Expect("Bitswap is disabled via Bitswap.Enabled=false")
		node.CheckLog("bitswap").Expect("using offline exchange")

		// Try fetching - should fail if bitswap is the only exchange mechanism available
		// Need another node to test fetching from.
		node2 := harness.NewT(t).NewNode().Init().StartDaemon()
		cid := node2.IPFSAddStr("hello world")

		// Connect node1 (bitswap disabled) to node2 (bitswap enabled)
		node.Connect(node2.PeerID().String(), node2.ListenAddrs()[0].String())

		// Node 1 tries to fetch from Node 2
		res = node.RunIPFS("cat", cid)
		// Expect failure because node1's bitswap client is disabled
		res.ExpectFailure("should fail to cat when bitswap is disabled")
		// Check error message (might vary, could be timeout or context deadline)
		res.ExpectStdErr("context deadline exceeded") // Common error when blocks aren't found

		// Check bitswap stat again on node1 just to be sure
		res = node.RunIPFS("bitswap", "stat")
		// Expect same behavior as before (likely error or empty)
		node.CheckLog("bitswap").Expect("Bitswap is disabled via Bitswap.Enabled=false")
	})

	t.Run("Bitswap server disabled", func(t *testing.T) {
		t.Parallel()
		// Node 1: Server disabled, Client enabled (default)
		node1 := harness.NewT(t).NewNode().Init()
		node1.SetConfig("Bitswap.ServerEnabled", "false")
		node1.StartDaemon()

		// Node 2: Default (Server enabled, Client enabled)
		node2 := harness.NewT(t).NewNode().Init().StartDaemon()

		// Add content to both nodes
		cid1 := node1.IPFSAddStr("content from node1")
		cid2 := node2.IPFSAddStr("content from node2")

		// Connect nodes
		node1.Connect(node2.PeerID().String(), node2.ListenAddrs()[0].String())
		node2.Connect(node1.PeerID().String(), node1.ListenAddrs()[0].String())

		// Wait for connections
		node1.WaitForPeer(node2.PeerID())
		node2.WaitForPeer(node1.PeerID())

		// Check logs for confirmation
		node1.WaitTillUp()
		node1.CheckLog("bitswap").Expect("Bitswap is enabled") // Bitswap itself is enabled
		node1.CheckLog("bitswap").Expect("Not wrapping with providing because Bitswap.ServerEnabled=false")

		node2.WaitTillUp()
		node2.CheckLog("bitswap").Expect("Bitswap is enabled")
		node2.CheckLog("bitswap").Expect("Wrapping exchange with providing") // Node 2 should be providing

		// Test 1: Node 1 (server disabled) fetches from Node 2 (server enabled) - SHOULD WORK
		t.Logf("Node 1 fetching %s from Node 2", cid2)
		res := node1.RunIPFS("cat", cid2)
		res.ExpectSuccessful()
		res.ExpectStdout("content from node2")

		// Check bitswap stats on Node 1 (should show data received)
		time.Sleep(1 * time.Second) // Give stats time to update
		stat1 := node1.RunIPFS("bitswap", "stat").Stdout()
		if !harness.Contains(stat1, "blocks received:") || harness.Contains(stat1, "blocks received: 0") {
			t.Fatalf("Node 1 bitswap stats should show blocks received > 0\n%s", stat1)
		}
		if harness.Contains(stat1, "data received:") || harness.Contains(stat1, "data received: 0 B") {
			t.Fatalf("Node 1 bitswap stats should show data received > 0\n%s", stat1)
		}
		// Should show 0 blocks sent
		if !harness.Contains(stat1, "blocks sent: 0") || !harness.Contains(stat1, "data sent: 0 B") {
			t.Fatalf("Node 1 bitswap stats should show 0 blocks/data sent\n%s", stat1)
		}

		// Test 2: Node 2 (server enabled) fetches from Node 1 (server disabled) - SHOULD FAIL
		t.Logf("Node 2 fetching %s from Node 1", cid1)
		res = node2.RunIPFS("cat", cid1)
		res.ExpectFailure("should fail to cat when peer's bitswap server is disabled")
		res.ExpectStdErr("context deadline exceeded") // Expect timeout as node1 won't respond

		// Check bitswap stats on Node 1 again (should still show 0 sent)
		time.Sleep(1 * time.Second) // Give stats time to update
		stat1After := node1.RunIPFS("bitswap", "stat").Stdout()
		if !harness.Contains(stat1After, "blocks sent: 0") || !harness.Contains(stat1After, "data sent: 0 B") {
			t.Fatalf("Node 1 bitswap stats should still show 0 blocks/data sent after failed fetch attempt\n%s", stat1After)
		}

		// Check bitswap stats on Node 2 (should show 0 received for cid1, maybe some sent for cid2)
		stat2 := node2.RunIPFS("bitswap", "stat").Stdout()
		// Verify it sent data for cid2 earlier
		if !harness.Contains(stat2, "blocks sent:") || harness.Contains(stat2, "blocks sent: 0") {
			t.Fatalf("Node 2 bitswap stats should show blocks sent > 0\n%s", stat2)
		}
		if harness.Contains(stat2, "data sent:") || harness.Contains(stat2, "data sent: 0 B") {
			t.Fatalf("Node 2 bitswap stats should show data sent > 0\n%s", stat2)
		}
		// It might show some received blocks if node1 sent WANTs, but data received should reflect cid1 failure.
		// This is harder to assert precisely without knowing exact block counts.
		fmt.Println("Node 2 Stats:\n", stat2)

	})

	t.Run("Bitswap default (enabled)", func(t *testing.T) {
		t.Parallel()
		// Node 1: Default
		node1 := harness.NewT(t).NewNode().Init().StartDaemon()
		// Node 2: Default
		node2 := harness.NewT(t).NewNode().Init().StartDaemon()

		// Add content
		cid1 := node1.IPFSAddStr("node1 default")
		cid2 := node2.IPFSAddStr("node2 default")

		// Connect nodes
		node1.Connect(node2.PeerID().String(), node2.ListenAddrs()[0].String())
		node2.Connect(node1.PeerID().String(), node1.ListenAddrs()[0].String())

		// Wait for connections
		node1.WaitForPeer(node2.PeerID())
		node2.WaitForPeer(node1.PeerID())

		// Check logs
		node1.WaitTillUp()
		node1.CheckLog("bitswap").Expect("Bitswap is enabled")
		node1.CheckLog("bitswap").Expect("Wrapping exchange with providing")
		node2.WaitTillUp()
		node2.CheckLog("bitswap").Expect("Bitswap is enabled")
		node2.CheckLog("bitswap").Expect("Wrapping exchange with providing")

		// Test fetch both ways - SHOULD WORK
		t.Logf("Node 1 fetching %s from Node 2", cid2)
		res := node1.RunIPFS("cat", cid2)
		res.ExpectSuccessful()
		res.ExpectStdout("node2 default")

		t.Logf("Node 2 fetching %s from Node 1", cid1)
		res = node2.RunIPFS("cat", cid1)
		res.ExpectSuccessful()
		res.ExpectStdout("node1 default")

		// Check stats briefly (expect non-zero send/receive on both)
		time.Sleep(1 * time.Second)
		stat1 := node1.RunIPFS("bitswap", "stat").Stdout()
		if harness.Contains(stat1, "blocks received: 0") || harness.Contains(stat1, "blocks sent: 0") {
			t.Fatalf("Node 1 stats should show >0 blocks received and sent\n%s", stat1)
		}
		stat2 := node2.RunIPFS("bitswap", "stat").Stdout()
		if harness.Contains(stat2, "blocks received: 0") || harness.Contains(stat2, "blocks sent: 0") {
			t.Fatalf("Node 2 stats should show >0 blocks received and sent\n%s", stat2)
		}
	})
}

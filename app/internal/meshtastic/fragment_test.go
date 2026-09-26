package meshtastic

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// TestFragmentRoundTrip — encode then decode through Reassembler should
// recover the exact original blob, regardless of size.
func TestFragmentRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		maxFrag  int
		expected int // expected fragment count
	}{
		{"empty", 0, 200, 1},
		{"single short", 50, 200, 1},
		{"single boundary", 193, 200, 1},      // 200-7=193 fits in one
		{"two", 194, 200, 2},                  // 1 byte over → 2 fragments
		{"max single packet", 200 - FragHeaderSize, 200, 1},
		{"five fragments", 5 * (200 - FragHeaderSize), 200, 5},
		{"large 5KB", 5000, 200, 26},          // ceil(5000 / 193)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob := make([]byte, tc.size)
			rand.Read(blob)

			frags, err := FragmentBlob(blob, tc.maxFrag)
			if err != nil {
				t.Fatalf("fragment: %v", err)
			}
			if len(frags) != tc.expected {
				t.Errorf("fragment count: got %d, want %d", len(frags), tc.expected)
			}

			r := NewReassembler(nil)
			var got []byte
			var done bool
			for i, f := range frags {
				out, complete, err := r.Feed(0xDEAD, f)
				if err != nil {
					t.Fatalf("feed %d: %v", i, err)
				}
				if complete {
					if i != len(frags)-1 {
						t.Errorf("completed early at frag %d/%d", i, len(frags))
					}
					got = out
					done = true
				}
			}
			if !done {
				t.Fatal("never completed")
			}
			if !bytes.Equal(got, blob) {
				t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(blob))
			}
		})
	}
}

// TestFragmentOutOfOrder — receiver should still complete when fragments
// arrive shuffled. LoRa rebroadcast can deliver out of order.
func TestFragmentOutOfOrder(t *testing.T) {
	blob := make([]byte, 1000)
	rand.Read(blob)

	frags, err := FragmentBlob(blob, 200)
	if err != nil {
		t.Fatalf("fragment: %v", err)
	}
	if len(frags) < 3 {
		t.Fatalf("test needs ≥3 fragments, got %d", len(frags))
	}

	// Reverse order delivery.
	r := NewReassembler(nil)
	var got []byte
	var done bool
	for i := len(frags) - 1; i >= 0; i-- {
		out, complete, err := r.Feed(0xBEEF, frags[i])
		if err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
		if complete {
			got = out
			done = true
		}
	}
	if !done {
		t.Fatal("never completed")
	}
	if !bytes.Equal(got, blob) {
		t.Error("payload mismatch on reversed delivery")
	}
}

// TestFragmentDuplicateIgnored — receiving the same fragment twice should
// not corrupt state or double-count. Replays are common in mesh routing
// (rebroadcast loops, duplicate paths).
func TestFragmentDuplicateIgnored(t *testing.T) {
	blob := make([]byte, 500)
	rand.Read(blob)
	frags, _ := FragmentBlob(blob, 200)
	if len(frags) < 2 {
		t.Fatal("need ≥2 fragments")
	}

	r := NewReassembler(nil)
	// Each fragment delivered, then immediately replayed. The dup must be
	// ignored — chunks[idx] is already filled, the received counter must
	// NOT increment. For all but the last fragment, both calls return
	// (nil, false). For the last fragment, the FIRST call completes;
	// the dup arriving after completion races with our cleanup but must
	// not crash or surface garbage.
	var out []byte
	var complete bool
	for i, f := range frags {
		out1, done1, err := r.Feed(1, f)
		if err != nil {
			t.Fatalf("first feed of frag %d errored: %v", i, err)
		}
		if i < len(frags)-1 && done1 {
			t.Fatalf("incorrectly completed at fragment %d", i)
		}
		if done1 {
			out = out1
			complete = true
		}
		// Replay — must not crash, must not return a different completion.
		out2, done2, err := r.Feed(1, f)
		if err != nil {
			t.Fatalf("dup of frag %d errored: %v", i, err)
		}
		if done2 && !bytes.Equal(out2, out1) {
			t.Errorf("dup of frag %d returned different completion", i)
		}
	}
	if !complete {
		t.Fatal("should complete after all fragments delivered")
	}
	if !bytes.Equal(out, blob) {
		t.Errorf("payload corrupted by duplicate fragments")
	}
}

// TestFragmentDifferentSenders — same msgID from different senders must not
// blend. Without per-source keying, two random senders that happen to pick
// the same msgID would corrupt each other.
func TestFragmentDifferentSenders(t *testing.T) {
	blob := make([]byte, 600)
	rand.Read(blob)
	frags, _ := FragmentBlob(blob, 200)

	// Re-extract msgID by reading the wire bytes back.
	if len(frags) < 2 {
		t.Fatal("need ≥2 fragments")
	}
	// Construct a fake fragment from "sender 2" reusing the same msgID
	// but with different total — this would corrupt sender 1's stream
	// if not isolated by source.
	fakeFrag := make([]byte, len(frags[0]))
	copy(fakeFrag, frags[0])
	fakeFrag[6] = 99 // wrong total

	r := NewReassembler(nil)
	r.Feed(0xAAA, frags[0]) // start sender A's message
	_, _, err := r.Feed(0xBBB, fakeFrag) // sender B sends conflicting fragment with same msgID
	// Sender B's fragment is invalid (idx=0 ok, but msg pending for B with total=99)
	// — should NOT affect sender A's reassembly.
	if err != nil && err != ErrInvalidFragment {
		t.Logf("sender B feed err: %v (expected nil or ErrInvalidFragment)", err)
	}
	// Now finish sender A — should reassemble cleanly despite B's noise.
	out, complete, err := r.Feed(0xAAA, frags[1])
	for i := 2; i < len(frags); i++ {
		if complete {
			break
		}
		out, complete, err = r.Feed(0xAAA, frags[i])
	}
	if err != nil {
		t.Fatalf("sender A failed despite B noise: %v", err)
	}
	if !complete {
		t.Fatal("sender A never completed")
	}
	if !bytes.Equal(out, blob) {
		t.Error("sender A payload corrupted by B")
	}
}

// TestFragmentInvalidInput — bad inputs produce specific errors, no panics.
func TestFragmentInvalidInput(t *testing.T) {
	r := NewReassembler(nil)

	if _, _, err := r.Feed(1, []byte{0x01}); err != ErrShortFragment {
		t.Errorf("short: want ErrShortFragment, got %v", err)
	}
	wrong := []byte{0x99, 0, 0, 0, 0, 0, 1}
	if _, _, err := r.Feed(1, wrong); err != ErrWrongType {
		t.Errorf("wrong type: want ErrWrongType, got %v", err)
	}
	idxOver := []byte{FragTypeEncryptedDM, 0, 0, 0, 0, 5, 3} // idx=5 ≥ total=3
	if _, _, err := r.Feed(1, idxOver); err != ErrInvalidFragment {
		t.Errorf("idx over total: want ErrInvalidFragment, got %v", err)
	}
}

// TestFragmentTooManyFragments — input requiring >255 fragments is rejected.
func TestFragmentTooManyFragments(t *testing.T) {
	huge := make([]byte, 256*100) // 256 fragments at 100 bytes each
	_, err := FragmentBlob(huge, 100+FragHeaderSize)
	if err == nil {
		t.Error("expected error for >255 fragments, got nil")
	}
}

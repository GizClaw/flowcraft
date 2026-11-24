package graph

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// numbered returns n messages whose text is their index, so a slice can be
// compared against the order it was appended in.
func numbered(n int) []message.Message {
	msgs := make([]message.Message, n)
	for i := range msgs {
		msgs[i] = message.NewTextMessage(message.RoleUser, fmt.Sprint(i))
	}
	return msgs
}

// appended builds a session the way a turn does: one Append per message.
func appended(msgs []message.Message) Messages {
	out := EmptyMessages()
	for _, msg := range msgs {
		out = out.Append(msg)
	}
	return out
}

// textsOf is the text of each message, which is how the tests compare order.
func textsOf(msgs []message.Message) []string {
	texts := make([]string, len(msgs))
	for i, msg := range msgs {
		texts[i] = msg.Content.Text()
	}
	return texts
}

// sliceTexts is the order Slice must report: oldest first.
func sliceTexts(m Messages) []string { return textsOf(m.Slice()) }

// eachTexts is the order Each must report: newest first.
func eachTexts(m Messages) []string {
	texts := make([]string, 0, m.Len())
	m.Each(func(msg message.Message) bool {
		texts = append(texts, msg.Content.Text())
		return true
	})
	return texts
}

func equalTexts(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A chain that ends exactly on, just below and just above a chunk boundary must
// keep its order and count, and appending across the boundary must not drop or
// duplicate a message.
func TestMessagesChunkBoundaries(t *testing.T) {
	for _, size := range []int{63, 64, 65, 127, 128, 129} {
		t.Run(fmt.Sprintf("messages=%d", size), func(t *testing.T) {
			msgs := numbered(size)
			session := appended(msgs)
			if got := session.Len(); got != size {
				t.Fatalf("Len = %d, want %d", got, size)
			}
			if got, want := sliceTexts(session), textsOf(msgs); !equalTexts(got, want) {
				t.Errorf("Slice = %v, want %v", got, want)
			}
			if got, want := eachTexts(session), reversed(textsOf(msgs)); !equalTexts(got, want) {
				t.Errorf("Each = %v, want %v", got, want)
			}

			// The session is still appendable after the boundary: the new message
			// lands at the end and the old ones keep their order.
			next := message.NewTextMessage(message.RoleUser, "next")
			grown := session.Append(next)
			if got := grown.Len(); got != size+1 {
				t.Fatalf("Len after Append = %d, want %d", got, size+1)
			}
			items := grown.Slice()
			if got := items[len(items)-1].Content.Text(); got != "next" {
				t.Errorf("last message = %q, want next", got)
			}
			if got := items[0].Content.Text(); got != "0" {
				t.Errorf("first message = %q, want 0", got)
			}
		})
	}
}

func reversed(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[len(values)-1-i] = value
	}
	return out
}

// Concat must survive joining chains whose tails have different lengths: the
// result is the left transcript followed by the right one, whichever side of a
// chunk boundary each ends on.
func TestMessagesConcatDifferentChunkLengths(t *testing.T) {
	for _, left := range []int{0, 1, 63, 64, 65, 130} {
		for _, right := range []int{0, 1, 63, 64, 65, 130} {
			t.Run(fmt.Sprintf("left=%d/right=%d", left, right), func(t *testing.T) {
				leftMsgs := numbered(left)
				rightMsgs := make([]message.Message, right)
				for i := range rightMsgs {
					rightMsgs[i] = message.NewTextMessage(message.RoleAssistant, fmt.Sprint(i))
				}
				leftSession, rightSession := appended(leftMsgs), appended(rightMsgs)
				got := leftSession.Concat(rightSession)
				if got.Len() != left+right {
					t.Fatalf("Len = %d, want %d", got.Len(), left+right)
				}
				want := append(textsOf(leftMsgs), textsOf(rightMsgs)...)
				if gotTexts := sliceTexts(got); !equalTexts(gotTexts, want) {
					t.Errorf("Slice = %v, want %v", gotTexts, want)
				}
				if gotTexts, wantTexts := eachTexts(got), reversed(want); !equalTexts(gotTexts, wantTexts) {
					t.Errorf("Each = %v, want %v", gotTexts, wantTexts)
				}
				// The operands are unchanged by the merge.
				if leftSession.Len() != left || rightSession.Len() != right {
					t.Error("Concat changed an operand")
				}
			})
		}
	}
}

// Each must stop where the callback says, and it must report the same messages in
// the same newest-first order however the chain was built.
func TestMessagesEachNewestFirst(t *testing.T) {
	msgs := numbered(200)
	session := appended(msgs)
	var got []string
	session.Each(func(msg message.Message) bool {
		got = append(got, msg.Content.Text())
		return len(got) < 3
	})
	if want := []string{"199", "198", "197"}; !equalTexts(got, want) {
		t.Fatalf("Each stopped at %v, want %v", got, want)
	}

	restored := MessagesFromSlice(msgs)
	if got, want := eachTexts(restored), reversed(textsOf(msgs)); !equalTexts(got, want) {
		t.Fatalf("Each on a restored session = %v, want %v", got, want)
	}
}

// Random appends, branches and restores must agree with a plain slice: Len,
// Slice order and Each order are the only observable state, so a model of
// them is enough to catch a lost or reordered chunk.
func TestMessagesMatchNaiveModel(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	type branch struct {
		session Messages
		model   []message.Message
	}
	branches := []branch{{session: EmptyMessages()}}
	for i := 0; i < 3_000; i++ {
		at := random.Intn(len(branches))
		parent := branches[at]

		var next branch
		switch roll := random.Intn(10); {
		case roll < 7 || len(parent.model) == 0:
			msg := message.NewTextMessage(message.RoleUser, fmt.Sprint(i))
			next = branch{session: parent.session.Append(msg), model: append(append([]message.Message{}, parent.model...), msg)}
		case roll < 9:
			msg := message.NewTextMessage(message.RoleAssistant, fmt.Sprint(i))
			other := MessagesFromSlice([]message.Message{msg})
			next = branch{session: parent.session.Concat(other), model: append(append([]message.Message{}, parent.model...), msg)}
		default:
			next = branch{session: MessagesFromSlice(parent.model), model: append([]message.Message{}, parent.model...)}
		}
		branches = append(branches, next)
		// Every branch is checked, not only the newest one.
		checkBranch(t, next.session, next.model)
		if len(branches) > 8 {
			branches = append(branches[:0], branches[len(branches)-8:]...)
		}
	}
}

func checkBranch(t *testing.T, session Messages, model []message.Message) {
	t.Helper()
	if got, want := session.Len(), len(model); got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	if got, want := sliceTexts(session), textsOf(model); !equalTexts(got, want) {
		t.Fatalf("Slice = %v, want %v", got, want)
	}
	if each, want := eachTexts(session), reversed(textsOf(model)); !equalTexts(each, want) {
		t.Fatalf("Each = %v, want %v", each, want)
	}
}

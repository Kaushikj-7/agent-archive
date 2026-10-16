package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// aheadChunk is one arrival at the terminal in loadAheadCase: keys pressed
// together, a read that fails, or Ctrl-Z.
type aheadChunk struct {
	data    string
	fail    bool
	suspend bool
}

// loadAheadCase is a load started with keys typed ahead (pending), keys and
// events arriving while it runs (during), and keys after it (after).
type loadAheadCase struct {
	pending string
	during  []aheadChunk
	after   []string
}

// countedKeys is a fakeKeys that closes reached once target chunks have been
// read, so that a load can end exactly when the keys pressed while it runs
// have arrived.
type countedKeys struct {
	*fakeKeys
	total   int
	target  int
	reached chan struct{}
	once    sync.Once
}

func (c *countedKeys) read(p []byte, wait time.Duration) (int, error) {
	n, err := c.fakeKeys.read(p, wait)
	c.mu.Lock()
	left := len(c.chunks)
	c.mu.Unlock()
	if c.total-left >= c.target {
		c.once.Do(func() { close(c.reached) })
	}
	return n, err
}

const aheadFail = "FAIL"

// aheadKeyNames names keys and failures for comparing streams.
func aheadKeyNames(keys []key) []string {
	var names []string
	for _, k := range keys {
		names = append(names, fmt.Sprintf("%d:%q", k.kind, k.r))
	}
	return names
}

func isLoadCancel(k key) bool {
	return k.kind == keyEscape || k.kind == keyEndOfInput || k.kind == keyRune && k.r == 'q'
}

// aheadTokens generates n key encodings. Esc is followed, in the same
// chunk, only by a move (an escape sequence) or by the chunk's end, which
// a key chunk joining its burst then starts with a move (see generate).
func aheadTokens(r *rand.Rand, n int, cancels bool) string {
	moves := []string{"\x1b[B", "\x1b[A", "\x1b[6~", "\x1b[5~", "\x1b[F", "\x1b[H"}
	typed := []string{"1", "2", "3", "7", "a", "b", "f", "/", " ", "\r", "\x7f"}
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch x := r.IntN(10); {
		case x < 4:
			b.WriteString(moves[r.IntN(len(moves))])
		case x < 8 || !cancels:
			b.WriteString(typed[r.IntN(len(typed))])
		default:
			switch r.IntN(3) {
			case 0:
				b.WriteString("\x1b")
				if i < n-1 {
					b.WriteString(moves[r.IntN(len(moves))])
					i++
				}
			case 1:
				b.WriteString("q")
			default:
				b.WriteString("\x04")
			}
		}
	}
	return b.String()
}

func generateLoadAheadCase(r *rand.Rand) loadAheadCase {
	c := loadAheadCase{pending: aheadTokens(r, 1+r.IntN(6), r.IntN(4) == 0)}
	for range r.IntN(6) {
		switch r.IntN(10) {
		case 0:
			c.during = append(c.during, aheadChunk{fail: true})
		case 1:
			c.during = append(c.during, aheadChunk{suspend: true})
		default:
			c.during = append(c.during, aheadChunk{data: aheadTokens(r, 1+r.IntN(5), r.IntN(3) == 0)})
		}
	}
	// A chunk ending in Esc is joined by the next to arrive within the wait
	// for an escape sequence's rest; that one starts with a move, so the
	// two do not read as Alt with a key.
	for i := 1; i < len(c.during); i++ {
		if prev, cur := c.during[i-1], c.during[i]; strings.HasSuffix(prev.data, "\x1b") && cur.data != "" && !strings.HasPrefix(cur.data, "\x1b[") {
			c.during[i].data = "\x1b[B" + cur.data
		}
	}
	for range r.IntN(3) {
		c.after = append(c.after, aheadTokens(r, 1+r.IntN(4), true))
	}
	return c
}

// aheadModel is what a load over c should do: whether a key read while it
// runs stops it (and quits), how many chunks it reads, and the keys and
// failures read after it, in order.
type aheadModel struct {
	cancel bool
	quit   bool
	read   int
	stream []string
}

func modelLoadAhead(c loadAheadCase) aheadModel {
	var m aheadModel
	var readKeys []key
	rest := func(from int) []string {
		var out []string
		var data []byte
		flush := func() {
			out = append(out, aheadKeyNames(decodeKeys(data))...)
			data = nil
		}
		for _, d := range c.during[from:] {
			switch {
			case d.fail:
				flush()
				out = append(out, aheadFail)
			case d.suspend:
			default:
				data = append(data, d.data...)
			}
		}
		flush()
		for _, a := range c.after {
			out = append(out, aheadKeyNames(decodeKeys([]byte(a)))...)
		}
		return out
	}
	pending := decodeKeys([]byte(c.pending))
	i := 0
	for i < len(c.during) {
		d := c.during[i]
		i++
		if d.suspend {
			continue
		}
		if d.fail {
			m.read = i
			m.stream = slices.Concat(aheadKeyNames(pending), aheadKeyNames(readKeys), []string{aheadFail}, rest(i))
			return m
		}
		data := []byte(d.data)
		failed := false
		for i < len(c.during) {
			next := c.during[i]
			if next.suspend {
				// Ctrl-Z ends the burst and is answered at once.
				i++
				break
			}
			if !incompleteKey(data) {
				break
			}
			i++
			if next.fail {
				failed = true
				break
			}
			data = append(data, next.data...)
		}
		keys := decodeKeys(data)
		if at := slices.IndexFunc(keys, isLoadCancel); at >= 0 {
			m.cancel = true
			m.quit = keys[at].kind != keyEscape
			m.read = i
			kept := slices.Concat(pending, readKeys, keys[:at], keys[at+1:])
			kept = slices.DeleteFunc(kept, func(k key) bool {
				_, move := scrollKeys[k.kind]
				return move
			})
			m.stream = aheadKeyNames(kept)
			if failed {
				m.stream = append(m.stream, aheadFail)
			}
			m.stream = append(m.stream, rest(i)...)
			return m
		}
		readKeys = append(readKeys, keys...)
		if failed {
			m.read = i
			m.stream = slices.Concat(aheadKeyNames(pending), aheadKeyNames(readKeys), []string{aheadFail}, rest(i))
			return m
		}
	}
	m.read = i
	m.stream = slices.Concat(aheadKeyNames(pending), aheadKeyNames(readKeys), rest(i))
	return m
}

// runLoadAhead runs a load over c and returns whether it was stopped (and
// quit), and the keys and failures read after it.
func runLoadAhead(t *testing.T, c loadAheadCase, m aheadModel) (cancelled, quit bool, stream []string) {
	t.Helper()
	var chunks []string
	for _, d := range c.during {
		switch {
		case d.fail:
			chunks = append(chunks, later(40*time.Millisecond, string(fakeFailure)))
		case d.suspend:
			chunks = append(chunks, string(fakeSuspend))
		default:
			chunks = append(chunks, later(40*time.Millisecond, d.data))
		}
	}
	chunks = append(chunks, c.after...)
	term := &countedKeys{fakeKeys: newFakeKeys(chunks...), total: len(chunks), target: m.read, reached: make(chan struct{})}
	if m.read == 0 {
		term.once.Do(func() { close(term.reached) })
	}
	keys := startKeys(term)
	defer keys.close()
	keys.pending = decodeKeys([]byte(c.pending))
	stuck := errors.New("the load was not stopped, or did not see the keys pressed while it ran")
	picker := &sessionPicker{keys: keys, older: &browserLoadAction{Label: "Older sessions", LoadContext: func(ctx context.Context) (bool, error) {
		if m.cancel {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(3 * time.Second):
				return false, stuck
			}
		}
		select {
		case <-term.reached:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(3 * time.Second):
			return false, stuck
		}
	}}}
	more, err := picker.loadOlderContext(context.Background(), nil, io.Discard)
	if err != nil || !more {
		t.Fatalf("%+v: load: %t, %v", c, more, err)
	}
	for {
		k, err := keys.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			stream = append(stream, aheadFail)
			continue
		}
		if k.kind != keyResize {
			stream = append(stream, aheadKeyNames([]key{k})...)
		}
	}
	return picker.operationCanceled, picker.operationQuit, stream
}

// A load started with keys typed ahead loses, repeats, and reorders no key
// pressed before, while, or after it runs, over random keys and timings: the
// first Esc, q, or Ctrl-D read while it runs stops it, dropping only the
// moves read by then, and a failed read comes after the keys before it,
// once. Ctrl-Z while it runs is answered and changes nothing else.
func TestLoadWithKeysTypedAheadKeepsEveryKeyInOrder(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(604, 3))
	for n := range 1500 {
		c := generateLoadAheadCase(r)
		m := modelLoadAhead(c)
		cancelled, quit, stream := runLoadAhead(t, c, m)
		if cancelled != m.cancel || quit != m.quit || !slices.Equal(stream, m.stream) {
			t.Fatalf("case %d %+v:\nstopped %t quit %t, want %t %t\nkeys %q\nwant %q", n, c, cancelled, quit, m.cancel, m.quit, stream, m.stream)
		}
	}
}

// Ctrl-Z pressed with keys while a load started with keys typed ahead runs
// is answered, and Esc pressed after it still stops the load.
func TestKeyPickerEscStopsALoadAfterCtrlZPressedWithKeys(t *testing.T) {
	t.Parallel()
	c := loadAheadCase{pending: "\x1b[B", during: []aheadChunk{{data: "1"}, {suspend: true}, {data: "\x1b"}}}
	m := modelLoadAhead(c)
	if !m.cancel {
		t.Fatal("the model does not stop the load")
	}
	cancelled, _, stream := runLoadAhead(t, c, m)
	if !cancelled || !slices.Equal(stream, m.stream) {
		t.Fatalf("stopped %t, keys %q, want %q", cancelled, stream, m.stream)
	}
}

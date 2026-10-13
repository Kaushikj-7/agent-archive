package cli

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// keyKind is what one key press on the browser's screens means.
type keyKind int

const (
	// keyRune is a printable character, space included; key.r holds it.
	keyRune keyKind = iota
	keyEnter
	keyBackspace
	// keyEscape is Esc pressed on its own.
	keyEscape
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPageUp
	keyPageDown
	keyHome
	keyEnd
	// keyEndOfInput is Ctrl-D, which ends input as it does at a line
	// prompt.
	keyEndOfInput
	// keyResize is not a key: the window changed size, so the screen is
	// drawn again.
	keyResize
)

// scrollMove is how a key scrolls a screen: a line, a screen, or to the
// top or bottom.
type scrollMove int

const (
	scrollLineUp scrollMove = iota
	scrollLineDown
	scrollPageUp
	scrollPageDown
	scrollTop
	scrollBottom
)

// scrollKeys are the keys that scroll, and how. Space scrolls a screen
// too, but is a typed character.
var scrollKeys = map[keyKind]scrollMove{
	keyUp:       scrollLineUp,
	keyDown:     scrollLineDown,
	keyPageUp:   scrollPageUp,
	keyPageDown: scrollPageDown,
	keyHome:     scrollTop,
	keyEnd:      scrollBottom,
}

// key is one decoded key press.
type key struct {
	kind keyKind
	r    rune
}

// errWindowResized is what a keyTerminal's read returns when the window
// changed size while it waited, and errSuspended when Ctrl-Z was pressed.
var (
	errWindowResized = errors.New("window resized")
	errSuspended     = errors.New("suspended")
)

// keyTerminal is a terminal read a key at a time: its line editing and
// echo can be turned off (keys) and back on (lines), keeping Ctrl-C and
// Ctrl-Z as signals. Env.openKeyTerminal supplies it; tests supply one that
// feeds raw bytes.
type keyTerminal interface {
	// keys turns line editing and echo off.
	keys() error
	// lines restores the modes the terminal had when it was opened.
	lines()
	// flush discards input not read yet.
	flush()
	// read waits up to wait (forever when wait is negative) for input and
	// returns what is available at once: several keys when the terminal
	// sent them together, as the mouse wheel does. It returns 0 bytes when
	// wait passes with no input, io.EOF when input ends, and
	// errWindowResized when the window changed size while it waited
	// forever, and errSuspended when Ctrl-Z was pressed while it waited.
	read(p []byte, wait time.Duration) (int, error)
	// stop stops the process, as Ctrl-Z does, and returns once it is
	// continued.
	stop() error
	// whilePaging answers Ctrl-Z while a pager runs, until end is called:
	// the pager stops itself, and so must the browser waiting for it, or
	// the shell never sees the job stop.
	whilePaging() (end func())
	// pendingStop reports, once, a Ctrl-Z that came while no key was
	// being read.
	pendingStop() bool
	// release ends what keys set up for the browser's whole run (Ctrl-Z
	// is its own from the first keys to release).
	release()
	// now is the time, for how long a split escape sequence is waited for.
	now() time.Time
}

// escapeWait is how long a lone Esc waits for the rest of an escape
// sequence: the bytes of one sequence arrive together, so an Esc followed
// by nothing within it was pressed on its own. It allows for a slow link
// (ssh, tmux, a loaded machine); a sequence split by more is still read as
// its key when the rest starts the next burst (see keyInput.stale).
const escapeWait = 120 * time.Millisecond

// staleFor is how long after it timed out the start of an escape sequence
// can still be joined by its rest. Later, what comes is typed.
const staleFor = 500 * time.Millisecond

// maxCarry is the longest tail cut off by maxBurst that is carried to the
// next read; a longer one is no key and is dropped.
const maxCarry = 16

// keyInput reads the browser's key presses. Keys read together (a wheel
// burst, or a pasted number) are queued, and a screen draws itself again
// only when the queue is empty, so a burst is applied before one redraw.
// It is safe to close from the interrupt handler while the browser reads.
type keyInput struct {
	term   keyTerminal
	mu     sync.Mutex
	on     bool
	closed bool
	// keep is set when closing leaves the input not read yet at the
	// terminal for the prompts after the browser (see handBack).
	keep    bool
	pending []key
	// failed is an error that came after keys still pending, returned once
	// they are handled.
	failed error
	// carry is the start of an escape sequence or character cut off by
	// maxBurst, read again with the next burst.
	carry []byte
	// stale is the start of an escape sequence that timed out at the end of
	// the last burst: a lone Esc, or ESC [ with no final byte. When the next
	// burst goes on with the rest of it, the two are read as one key.
	stale   []byte
	staleAt time.Time
	// hide and show, when set, leave the alternate screen before Ctrl-Z
	// stops the process and enter it again once it continues.
	hide func()
	show func()
}

// startKeys turns key mode on, or returns nil (line input) when it cannot.
func startKeys(term keyTerminal) *keyInput {
	k := &keyInput{term: term}
	if k.resume() != nil {
		k.close()
		return nil
	}
	return k
}

// resume turns key mode (back) on. It does nothing once closed, so the
// interrupt handler's restore cannot be undone by the browser.
func (k *keyInput) resume() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed || k.on {
		return nil
	}
	if err := k.term.keys(); err != nil {
		k.term.lines()
		return err
	}
	k.on = true
	return nil
}

// suspend restores line input, as for a pager.
func (k *keyInput) suspend() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.on {
		k.on = false
		k.term.lines()
	}
}

// close restores line input for good, discarding keys not read yet (the
// rest of a wheel's momentum, say) so the shell does not get them, unless
// handBack kept them for the prompts after the browser. It may be called more
// than once.
func (k *keyInput) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.closed = true
	if !k.keep {
		k.term.flush()
	}
	if k.on {
		k.on = false
		k.term.lines()
	}
	k.term.release()
}

// handBack gives the prompts after the browser what the person typed ahead of
// them: the typed text that was read with the key that ended the browser, and
// what is still waiting at the terminal, which close then leaves there. It
// reads the keys' input only: a key that moves, scrolls, or acts is dropped,
// so a wheel's momentum is not typed into the next prompt. Backspace takes
// back the character before it, as the terminal's own line editing would, and
// Enter ends a line.
func (k *keyInput) handBack(give func([]byte)) {
	rest := k.unused()
	k.mu.Lock()
	k.keep = true
	k.mu.Unlock()
	if len(rest) > 0 {
		give(rest)
	}
}

// unused is the text typed into the keys' input that has not been used: the
// keys read already, and whatever else has arrived at the terminal.
func (k *keyInput) unused() []byte {
	data := k.carry
	k.carry = nil
	buf := make([]byte, 256)
	for len(data) < maxBurst {
		n, err := k.term.read(buf, 0)
		data = append(data, buf[:n]...)
		if n == 0 || err != nil {
			break
		}
	}
	keys := slices.Concat(k.pending, k.decode(data))
	k.pending = nil
	var text []byte
	for _, key := range keys {
		switch key.kind {
		case keyRune:
			text = utf8.AppendRune(text, key.r)
		case keyEnter:
			text = append(text, '\n')
		case keyBackspace:
			if n := len(text); n > 0 && text[n-1] != '\n' {
				_, size := utf8.DecodeLastRune(text)
				text = text[:n-size]
			}
		case keyEscape, keyUp, keyDown, keyLeft, keyRight, keyPageUp, keyPageDown, keyHome, keyEnd, keyEndOfInput, keyResize:
			// Not typed text.
		}
	}
	return text
}

// page gives a pager the terminal: line input back, and Ctrl-Z answered
// while it runs. end ends that; resume turns keys back on after it.
//
// A Ctrl-Z that came before (while the transcript loaded, say) is answered
// first, with the browser's screen put away: the watcher would stop the
// process at once, before the pager ran, with the alternate screen up.
func (k *keyInput) page() (end func()) {
	if k.term.pendingStop() {
		// An error setting the modes again leaves line input, which the
		// pager wants anyway; resume after it reports it.
		_ = k.pause()
	}
	k.suspend()
	return k.term.whilePaging()
}

// buffered reports whether keys already read wait to be handled.
func (k *keyInput) buffered() bool {
	return len(k.pending) > 0
}

// next returns the next key press, waiting for one.
func (k *keyInput) next() (key, error) { return k.nextWait(-1) }

func (k *keyInput) nextWait(wait time.Duration) (key, error) {
	for len(k.pending) == 0 {
		// An error that ended the last burst (Ctrl-Z during the wait for
		// an escape sequence's rest, say) is handled as a read's would be.
		var data []byte
		err := k.failed
		k.failed = nil
		if err == nil {
			data, err = k.readBurstWait(wait)
			if err == nil && len(data) == 0 && wait >= 0 {
				return key{kind: keyResize}, nil
			}
		}
		if err != nil {
			// What came before a resize or Ctrl-Z is not continued after.
			k.stale = nil
		}
		if errors.Is(err, errSuspended) {
			if err := k.pause(); err != nil {
				return key{}, err
			}
			// Back from Ctrl-Z: the screen is drawn again.
			return key{kind: keyResize}, nil
		}
		if errors.Is(err, errWindowResized) {
			return key{kind: keyResize}, nil
		}
		if err != nil {
			return key{}, err
		}
		k.pending = k.decode(data)
	}
	next := k.pending[0]
	k.pending = k.pending[1:]
	return next, nil
}

// pause answers Ctrl-Z: the terminal gets its modes and its normal screen
// back while the process is stopped, and the browser's once it continues.
//
// Key mode goes back on before the browser's screen: continued in the
// background (bg), setting the terminal's modes stops the process again
// (SIGTTOU) until it is in the foreground, so the screen is not drawn
// over the shell's prompt.
func (k *keyInput) pause() error {
	k.suspend()
	if k.hide != nil {
		k.hide()
	}
	err := k.term.stop()
	if err == nil {
		err = k.resume()
	}
	if k.show != nil {
		k.show()
	}
	return err
}

// decode turns a burst into keys. A burst going on with an escape sequence
// that timed out at the end of the last one (ESC, then [B after a pause)
// is read as that sequence's key, not typed; ESC O goes on only with a key
// it names, since O may be typed after Esc.
func (k *keyInput) decode(data []byte) []key {
	stale := k.stale
	k.stale = nil
	if len(stale) > 0 && k.term.now().Sub(k.staleAt) > staleFor {
		stale = nil
	}
	if len(stale) > 0 && len(data) > 0 && (len(stale) > 1 || data[0] == '[' || data[0] == 'O') {
		joined := append(append([]byte(nil), stale...), data...)
		if found, size, complete := decodeEscape(joined); complete && size > len(stale) && (found != nil || joined[1] == '[') {
			var keys []key
			if found != nil {
				keys = append(keys, *found)
			}
			return append(keys, k.decode(joined[size:])...)
		}
	}
	if tail := incompleteTail(data); tail > 0 && data[len(data)-tail] == 0x1b {
		// Waited out: decoded below as a lone Esc or dropped, and kept in
		// case the rest comes next.
		k.stale = append([]byte(nil), data[len(data)-tail:]...)
		k.staleAt = k.term.now()
	}
	return decodeKeys(data)
}

// maxBurst bounds the bytes read as one burst.
const maxBurst = 4096

// readBurst waits for input, then takes whatever else is already there,
// and waits briefly for the rest of an escape sequence cut off at its end.
func (k *keyInput) readBurst() ([]byte, error) { return k.readBurstWait(-1) }

func (k *keyInput) readBurstWait(wait time.Duration) ([]byte, error) {
	buf := make([]byte, 256)
	n, err := k.term.read(buf, wait)
	if err != nil {
		return nil, err
	}
	data := k.readRest(append(k.carry, buf[:n]...), buf)
	k.carry = nil
	if tail := incompleteTail(data); tail > 0 && len(data) >= maxBurst {
		// Cut off by the cap, not by time: the rest is already waiting.
		if tail <= maxCarry {
			k.carry = append([]byte(nil), data[len(data)-tail:]...)
		}
		data = data[:len(data)-tail]
	}
	return data, nil
}

// readRest adds to data what else has arrived with it. An error ends the
// burst; it is kept in k.failed for after the keys already read.
func (k *keyInput) readRest(data, buf []byte) []byte {
	for len(data) < maxBurst {
		wait := time.Duration(0)
		if incompleteKey(data) {
			wait = escapeWait
		}
		n, err := k.term.read(buf, wait)
		data = append(data, buf[:n]...)
		k.failed = err
		if err != nil || n == 0 {
			break
		}
	}
	return data
}

// incompleteKey reports whether data ends partway through an escape
// sequence or a UTF-8 character.
func incompleteKey(data []byte) bool {
	return incompleteTail(data) > 0
}

// incompleteTail is how many bytes at the end of data are an escape
// sequence or a UTF-8 character cut off, or 0.
func incompleteTail(data []byte) int {
	if i := bytes.LastIndexByte(data, 0x1b); i >= 0 {
		if _, size, complete := decodeEscape(data[i:]); !complete && i+size == len(data) {
			return len(data) - i
		}
	}
	// A UTF-8 lead byte whose continuation bytes have not arrived.
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if data[i] >= utf8.RuneSelf && !utf8.FullRune(data[i:]) {
				return len(data) - i
			}
			return 0
		}
	}
	return 0
}

// decodeKeys turns raw terminal input into key presses. Escape sequences it
// does not know, and control characters, are dropped, never echoed.
func decodeKeys(data []byte) []key {
	var keys []key
	for len(data) > 0 {
		switch b := data[0]; {
		case b == 0x1b:
			k, size, _ := decodeEscape(data)
			if k != nil {
				keys = append(keys, *k)
			}
			data = data[size:]
			continue
		case b == '\r' || b == '\n':
			keys = append(keys, key{kind: keyEnter})
			if b == '\r' && len(data) > 1 && data[1] == '\n' {
				data = data[1:]
			}
		case b == 0x7f || b == 0x08:
			keys = append(keys, key{kind: keyBackspace})
		case b == 0x04:
			keys = append(keys, key{kind: keyEndOfInput})
		case b < 0x20:
			// Another control character: ignored.
		default:
			r, size := utf8.DecodeRune(data)
			if r != utf8.RuneError || size > 1 {
				keys = append(keys, key{kind: keyRune, r: r})
			}
			data = data[size:]
			continue
		}
		data = data[1:]
	}
	return keys
}

// csiKeys are the keys of ESC [ sequences ending in a letter, and of
// ESC O (application cursor mode).
var csiKeys = map[byte]keyKind{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft, 'H': keyHome, 'F': keyEnd}

// tildeKeys are the keys of ESC [ N ~ sequences.
var tildeKeys = map[int]keyKind{1: keyHome, 7: keyHome, 4: keyEnd, 8: keyEnd, 5: keyPageUp, 6: keyPageDown}

// decodeEscape decodes the escape sequence data starts with. It returns
// the key (nil for a sequence it ignores), how many bytes the sequence
// takes, and whether it was complete: an Esc at the end of data is a lone
// Esc, and a sequence cut off is dropped.
func decodeEscape(data []byte) (k *key, size int, complete bool) {
	if len(data) == 1 {
		return &key{kind: keyEscape}, 1, false
	}
	switch data[1] {
	case 0x1b:
		// Esc pressed twice: the first on its own.
		return &key{kind: keyEscape}, 1, true
	case 'O':
		if len(data) < 3 {
			return nil, len(data), false
		}
		if kind, ok := csiKeys[data[2]]; ok {
			return &key{kind: kind}, 3, true
		}
		return nil, 3, true
	case '[':
		return decodeCSI(data)
	}
	// Alt with a key: ignored.
	_, n := utf8.DecodeRune(data[1:])
	return nil, 1 + n, true
}

// decodeCSI decodes an ESC [ sequence: parameter bytes, intermediate
// bytes, then a final byte.
func decodeCSI(data []byte) (*key, int, bool) {
	i := 2
	for i < len(data) && data[i] >= 0x30 && data[i] <= 0x3f {
		i++
	}
	params := string(data[2:i])
	for i < len(data) && data[i] >= 0x20 && data[i] <= 0x2f {
		i++
	}
	if i == len(data) {
		return nil, len(data), false
	}
	final := data[i]
	size := i + 1
	if final < 0x40 || final > 0x7e {
		// Not a sequence after all: drop the Esc and [ only.
		return nil, 2, true
	}
	if final == '~' {
		n, err := strconv.Atoi(params)
		if kind, ok := tildeKeys[n]; ok && err == nil {
			return &key{kind: kind}, size, true
		}
		return nil, size, true
	}
	if kind, ok := csiKeys[final]; ok && (params == "" || params == "1") {
		return &key{kind: kind}, size, true
	}
	return nil, size, true
}

// openTerminalKeys opens stdin to be read a key at a time: terminalKeys.
// The package's tests replace it with one that never does, so no test
// reads or changes the developer's own terminal unless it sets
// Env.openKeys.
var openTerminalKeys = terminalKeys

// openKeyTerminal returns stdin as a keyTerminal when it is a terminal
// that can be read a key at a time.
func (e Env) openKeyTerminal(stdin io.Reader) (keyTerminal, bool) {
	if e.openKeys != nil {
		return e.openKeys(stdin)
	}
	return openTerminalKeys(stdin)
}

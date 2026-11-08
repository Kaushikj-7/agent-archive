package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// A malformed private selector is refusal evidence only. encoding/json skips
// custom unmarshaling on syntax errors, so typed raw readers classify it first.
func decodeNativeReservationIndex(raw []byte, entry *qualifiedSessionIndexEntry) error {
	err := json.Unmarshal(raw, entry)
	if err != nil && nativeReservationKeyPossible(raw) {
		return errors.Join(errUnsupportedNativeReservation, err)
	}
	return err
}

func nativeReservationKeyPossible(raw []byte) bool {
	const key = "native_reservation"
	const maxKeyWire = len(key)*6 + 2
	for start, ch := range raw {
		if ch != '"' || nativeReservationQuoteEscaped(raw, start) {
			continue
		}
		for end := start + 1; end < len(raw) && end-start < maxKeyWire; end++ {
			if raw[end] != '"' || nativeReservationQuoteEscaped(raw, end) {
				continue
			}
			suffix := bytes.TrimLeftFunc(raw[end+1:], unicode.IsSpace)
			if len(suffix) == 0 || suffix[0] != ':' {
				break
			}
			var decoded string
			if json.Unmarshal(raw[start:end+1], &decoded) == nil && strings.EqualFold(decoded, key) {
				return true
			}
			break
		}
	}
	return false
}

func nativeReservationQuoteEscaped(raw []byte, at int) bool {
	count := 0
	for at--; at >= 0 && raw[at] == '\\'; at-- {
		count++
	}
	return count%2 != 0
}

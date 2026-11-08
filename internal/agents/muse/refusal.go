package muse

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// refusal names the native record family a strict decode refused at line.
// Only failures of the closed native format are named; budget, cancellation
// and I/O errors pass through unchanged.
func refusal(raw []byte, line int64, err error) error {
	if !errors.Is(err, errFormat) {
		return err
	}
	var native struct {
		Frame       *string `json:"retained_frame"`
		PayloadType string  `json:"payload_type"`
		Payload     struct {
			Kind  any `json:"kind"`
			Event struct {
				Kind any `json:"kind"`
			} `json:"event"`
			Record struct {
				Kind any `json:"kind"`
			} `json:"record"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &native) != nil {
		return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, "", line, err)
	}
	if native.Frame != nil {
		return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, *native.Frame, line, err)
	}
	family := ""
	if agentapi.ReportableNativeFamily(native.PayloadType) {
		family = native.PayloadType
		// A kind adds to the family only when it is a distinct reportable name;
		// one that is not keeps the payload type rather than blanking it.
		parts := []string{native.PayloadType}
		for _, kind := range []any{native.Payload.Kind, native.Payload.Event.Kind, native.Payload.Record.Kind} {
			if s, ok := kind.(string); ok && s != "" && !slices.Contains(parts, s) && agentapi.ReportableNativeFamily(family+"/"+s) {
				family += "/" + s
				parts = append(parts, s)
			}
		}
	}
	if line == 1 && family != "" {
		// The session layout's first line is always the permission frame. A file
		// starting with an ordinary record is a native side log, not a session.
		return agentapi.NewNativeRefusalError(agentapi.RefusalNoSessionStart, family, line, err)
	}
	return agentapi.NewNativeRefusalError(agentapi.RefusalUnsupportedRecord, family, line, err)
}

// rootError is errFormat naming the logical row that failed root validation,
// or row -1 when no single row did. order reports a startup structure the
// adapter has not qualified, rather than a row contradicting the session.
type rootError struct {
	row   int
	order bool
}

func (rootError) Error() string { return errFormat.Error() }

func (rootError) Unwrap() error { return errFormat }

// startupRefusal names why rows ending at physical line did not qualify as a
// session. Rows short of their startup lane are incomplete unless a present
// row is itself inconsistent. Other errors pass through unchanged.
func startupRefusal(rows []logicalRow, line int64, err error) error {
	var failed rootError
	if !errors.As(err, &failed) {
		return err
	}
	supported := supportedRows(rows)
	if failed.order && len(supported) < startupRows(supported) {
		return agentapi.NewNativeRefusalError(agentapi.RefusalIncompleteStart, "", line, err)
	}
	reason := agentapi.RefusalInconsistentRecord
	if failed.order {
		reason = agentapi.RefusalUnsupportedStartup
	}
	family := ""
	if failed.row < 0 && len(supported) > 0 {
		// Only the whole startup prefix failed: name the row that completed it.
		last := supported[len(supported)-1]
		family, line = string(last.Type), last.Position.Line
	} else if failed.row >= 0 {
		r := rows[failed.row]
		if r.ordinary != nil {
			family = string(r.ordinary.Type)
		} else if r.omission != nil {
			family = r.omission.Omitted.PayloadType
		}
		if r.valid() {
			_, _, _, pos := r.identity()
			line = pos.Line
		}
	}
	return agentapi.NewNativeRefusalError(reason, family, line, err)
}

// shortRefusal names why rows, read to line before end stopped the read short
// of their startup lane, did not qualify. end stays in the error chain.
func shortRefusal(rows []logicalRow, line int64, end error) error {
	err := validateRoot(rows)
	if err == nil {
		err = rootError{row: -1, order: true}
	}
	return startupRefusal(rows, line, errors.Join(end, err))
}

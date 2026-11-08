package agentapi

import (
	"fmt"
	"regexp"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// NativeRefusalReason names why a strict native adapter refused a whole file.
type NativeRefusalReason string

// Native refusal reasons are fixed codes; callers word the next step.
const (
	// RefusalUnsupportedRecord reports the first record the adapter does not
	// support, either an unknown family or a known one in a newer shape.
	RefusalUnsupportedRecord NativeRefusalReason = "unsupported_record"
	// RefusalNoSessionStart reports a file in the session layout whose first
	// record does not start a session, such as a native side log.
	RefusalNoSessionStart NativeRefusalReason = "no_session_start"
	// RefusalIncompleteStart reports a session file that ends before its
	// startup records, such as one holding only the permission frame.
	RefusalIncompleteStart NativeRefusalReason = "incomplete_start"
	// RefusalUnsupportedStartup reports startup records that are each
	// supported but arrive in an order the adapter has not qualified.
	RefusalUnsupportedStartup NativeRefusalReason = "unsupported_startup"
	// RefusalInconsistentRecord reports a supported record that contradicts
	// the session's other records, such as another owner, workspace or
	// duplicate record ID.
	RefusalInconsistentRecord NativeRefusalReason = "inconsistent_record"
)

// nativeFamily is the native schema identifier shape: lowercase words joined
// by dots or underscores, with only versions such as "v3" carrying digits.
// Anything else, including a forged family carrying a token, is not reported.
var nativeFamily = regexp.MustCompile(`^[a-z]+(?:[._/](?:[a-z]+|v[0-9]{1,3}))*$`)

const maxNativeFamily = 96

// NativeRefusalError reports a whole-file refusal with a bounded native record
// family and physical line. It never carries record content.
type NativeRefusalError struct {
	Reason NativeRefusalReason
	// Family is the native record family, or empty when it is not a plain
	// schema identifier.
	Family string
	Line   int64
	Err    error
}

// ReportableNativeFamily reports whether family has the schema identifier
// shape and the shared credential redaction leaves it unchanged.
func ReportableNativeFamily(family string) bool {
	_, changed := archive.RedactSensitive(family)
	return !changed && len(family) <= maxNativeFamily && nativeFamily.MatchString(family)
}

// NewNativeRefusalError keeps family only when it is reportable.
func NewNativeRefusalError(reason NativeRefusalReason, family string, line int64, err error) *NativeRefusalError {
	if !ReportableNativeFamily(family) {
		family = ""
	}
	return &NativeRefusalError{Reason: reason, Family: family, Line: line, Err: err}
}

func (e *NativeRefusalError) Error() string {
	family := e.Family
	if family == "" {
		family = "unrecognized"
	}
	switch e.Reason {
	case RefusalNoSessionStart:
		return fmt.Sprintf("not a session: first record is %s, not a session start", family)
	case RefusalIncompleteStart:
		return fmt.Sprintf("incomplete session: the file ends at line %d, before its startup records", e.Line)
	case RefusalUnsupportedStartup:
		return fmt.Sprintf("unsupported startup order: %s at line %d", family, e.Line)
	case RefusalInconsistentRecord:
		return fmt.Sprintf("inconsistent session: %s at line %d contradicts its other records", family, e.Line)
	case RefusalUnsupportedRecord:
	}
	return fmt.Sprintf("unsupported %s record at line %d", family, e.Line)
}

func (e *NativeRefusalError) Unwrap() error { return e.Err }

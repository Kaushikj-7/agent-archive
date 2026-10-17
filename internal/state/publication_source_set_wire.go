package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"reflect"
	"strings"
)

// Closed protocols never let future authority fall through to a legacy record.
func closedPublicationDecode(data []byte, dst any, fields ...map[string]bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniquePublicationJSON(decoder, 0, reflect.TypeOf(dst), fields...); err != nil {
		// Direct budgeted entrypoints do not receive Unmarshal's outer syntax
		// preflight. A semantic refusal can precede later malformed bytes. Recover
		// the same stdlib syntax error without decoding an authority object.
		var ignored struct{}
		if syntaxErr := json.Unmarshal(data, &ignored); syntaxErr != nil {
			var syntax *json.SyntaxError
			if errors.As(syntaxErr, &syntax) {
				err = syntaxErr
			}
		}
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	// Preserve trailing-data refusal before typed decoding, including its
	// non-quarantine disposition for an otherwise complete JSON value.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrDurableStorageRecovery
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func decodeSourceSetPendingPublication(data []byte, p *PendingPublication, ctx context.Context, budget *agentapi.NativeReadBudget, owners ...stateReservation) error {
	type pendingJSON PendingPublication
	var decoded pendingJSON
	fields := make(map[string]bool)
	if err := closedPublicationDecode(data, &decoded, fields); err != nil {
		return err
	}
	*p = PendingPublication(decoded)
	if p.JournalVersion == 5 {
		if p.Selection != nil || p.StagedProof() != nil {
			return ErrDurableStorageRecovery
		}
		if fields["source_bytes"] {
			return ErrDurableStorageRecovery
		}
		if len(p.Sources) > 0 && p.Sources[0].Payload.Kind == PublicationInline {
			p.SourceBytes = p.Sources[0].Payload.Inline
		}
		count := len(p.Sources)
		if p.Preparation != nil {
			count += len(p.Preparation.Inputs)
		}
		facts, release, err := newPayloadDigestFacts(ctx, budget, count, owners...)
		if err != nil {
			return err
		}
		defer release()
		if err := p.validatePublicationEnvelopeWithFacts(facts); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	} else {
		for _, field := range []string{"journal_version", "phase", "preparation", "progress", "cleanup"} {
			if fields[field] {
				return ErrDurableStorageRecovery
			}
		}
		if p.JournalVersion != 0 || p.Phase != "" || p.Preparation != nil || p.Progress != nil || p.Cleanup != nil || p.Commit != nil && p.Commit.Version != 1 {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func decodeSourceSetPublishedState(data []byte, p *publishedState, ctx context.Context, budget *agentapi.NativeReadBudget, owners ...stateReservation) error {
	type publishedJSON publishedState
	var decoded publishedJSON
	fields := make(map[string]bool)
	if err := closedPublicationDecode(data, &decoded, fields); err != nil {
		return err
	}
	*p = publishedState(decoded)
	if p.PublicationVersion == 5 {
		if p.LastPublished != nil && (p.LastPublished.Selection != nil || p.LastPublished.StagedProof() != nil) {
			return ErrDurableStorageRecovery
		}
		count := len(p.Payloads)
		if p.Preparation != nil {
			count += len(p.Preparation.Inputs)
		}
		facts, release, err := newPayloadDigestFacts(ctx, budget, count, owners...)
		if err != nil {
			return err
		}
		defer release()
		return p.validateSelectingPublishedWithFacts(facts)
	}
	for _, field := range []string{"publication_version", "settled_privacy", "preparation", "payloads", "cleanup", "privacy_receipts"} {
		if fields[field] {
			return ErrDurableStorageRecovery
		}
	}
	// Claimed authority cannot disappear through null/empty JSON values. A
	// complete legacy Commit1 is the only supported legacy selecting variant.
	if fields["source_set_commit5"] || fields["sources"] || fields["predecessor_unknown"] {
		if p.Commit == nil || p.Commit.Version != 1 || len(p.Sources) == 0 || p.PredecessorUnknown {
			return ErrDurableStorageRecovery
		}
		bundle, _, found := p.resolveLastPublished()
		if !found || len(p.MetadataBytes) == 0 {
			return ErrDurableStorageRecovery
		}
		active := p.Sources[0]
		var metadata archive.Metadata
		if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}

		key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
		if err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
		sources := make([]PublicationSource, len(p.Sources))
		for i, ref := range p.Sources {
			sources[i].Reference = ref
		}
		pending := PendingPublication{MetadataKey: key, Commit: p.Commit, Sources: sources, Bundle: bundle, SourceKey: active.Key, SourceSHA256: active.SHA256, SourceSize: active.CompressedBytes, MetadataOnly: true, MetadataBytes: p.MetadataBytes}
		if err := pending.validateReadyPublication(); err != nil {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
	}
	if p.PublicationVersion != 0 || p.SettledPrivacy != nil || p.Preparation != nil || len(p.Payloads) != 0 || p.Cleanup != nil || p.Commit != nil && p.Commit.Version != 1 {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (p publishedState) validateSelectingPublished() error {
	return p.validateSelectingPublishedWithFacts(nil)
}

func (p publishedState) validateSelectingPublishedWithFacts(facts *payloadDigestFacts) (err error) {
	defer func() { err = facts.result(err) }()
	derived := p.summary()
	if p.Summary == nil || !reflect.DeepEqual(*p.Summary, derived) {
		return ErrDurableStorageRecovery
	}
	if p.PublicationVersion != 5 || p.Commit == nil || p.Commit.Version != 2 || p.Commit.PayloadSetSHA256 != payloadSetSHAWithFacts(p.Payloads, facts) || p.Commit.MetadataSHA256 != publicationSHA256(p.MetadataBytes) || p.Commit.PrivacySHA256 != privacyReceiptsSHA(p.PrivacyReceipts) {
		return ErrDurableStorageRecovery
	}
	if (p.Preparation == nil) == (p.SettledPrivacy == nil) {
		return ErrDurableStorageRecovery
	}
	if err := p.validateSelectingPreparationWithFacts(facts); err != nil {
		return err
	}
	// The selecting bundle survives cache/block candidates in LastPublished.
	bundle, _, found := p.resolveLastPublished()
	if !found {
		return ErrDurableStorageRecovery
	}
	if len(p.Payloads) == 0 {
		return ErrDurableStorageRecovery
	}
	current := p.Payloads[0].Reference
	commit := *p.Commit
	commit.SettledPrivacySHA256 = ""

	// Published validation uses private Commit2+payload witnesses without a mutable preparation cursor.
	var metadata struct {
		SourceBundle    archive.SourceReference `json:"source_bundle"`
		ParentSessionID string                  `json:"parent_session_id"`
		NativeChild     bool                    `json:"native_child"`
		SessionID       string                  `json:"session_id"`
		Harness         struct {
			Name string `json:"name"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return err
	}
	if metadata.SourceBundle != current || p.LastPublished != nil && p.LastPublished.Source != nil && *p.LastPublished.Source != current {
		return ErrDurableStorageRecovery
	}
	var target *PublicationNativeTarget
	if p.Preparation != nil {
		target = p.Preparation.NativeTarget
	} else if p.SettledPrivacy != nil {
		target = p.SettledPrivacy.NativeTarget
	}
	if target != nil && (metadata.ParentSessionID != target.ParentSessionID || metadata.NativeChild != target.NativeChild || bundle.ParentSessionID != target.ParentSessionID || bundle.NativeChild != target.NativeChild) {
		return ErrDurableStorageRecovery
	}
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		return err
	}
	pending := PendingPublication{MetadataKey: key, JournalVersion: 5, Commit: &commit, Sources: p.Payloads, Bundle: bundle, SourceKey: current.Key, SourceSHA256: current.SHA256, SourceSize: current.CompressedBytes, MetadataOnly: true, MetadataBytes: p.MetadataBytes, SourceBytes: p.Payloads[0].Payload.Inline}
	if err := pending.validateReadyPublicationWithFacts(facts); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

// uniquePublicationJSON rejects duplicate authority keys before typed decoding.
func uniquePublicationJSON(d *json.Decoder, depth int, destination reflect.Type, topFields ...map[string]bool) error {
	for destination != nil && destination.Kind() == reflect.Pointer {
		destination = destination.Elem()
	}
	if depth > 128 {
		return ErrDurableStorageRecovery
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			var child reflect.Type
			// Go's typed JSON decoder accepts case aliases for struct fields.
			// Canonicalize only those fields; arbitrary native map keys remain exact.
			name, child = publicationJSONField(destination, name)
			if destination != nil && destination.Kind() == reflect.Struct && child == nil {
				return ErrDurableStorageRecovery
			}
			if depth == 0 && len(topFields) > 0 {
				name = strings.ToLower(name)
			}
			if !ok || keys[name] {
				return ErrDurableStorageRecovery
			}
			keys[name] = true
			if depth == 0 && len(topFields) > 0 {
				topFields[0][name] = true
			}
			if err = uniquePublicationJSON(d, depth+1, child); err != nil {
				return err
			}
		}
	case '[':
		var child reflect.Type
		if destination != nil && (destination.Kind() == reflect.Slice || destination.Kind() == reflect.Array) {
			child = destination.Elem()
		}
		for d.More() {
			if err = uniquePublicationJSON(d, depth+1, child); err != nil {
				return err
			}
		}
	default:
		return ErrDurableStorageRecovery
	}
	_, err = d.Token()
	return err
}

func publicationJSONField(destination reflect.Type, name string) (string, reflect.Type) {
	var child reflect.Type
	if destination != nil && destination.Kind() == reflect.Struct {
		for i := range destination.NumField() {
			field := destination.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag == "" {
				tag = field.Name
			}
			if strings.EqualFold(name, tag) {
				return tag, field.Type
			}
		}
	} else if destination != nil && destination.Kind() == reflect.Map {
		child = destination.Elem()
	}
	return name, child
}

func (p publishedState) validateSelectingPreparationWithFacts(facts *payloadDigestFacts) error {
	if p.SettledPrivacy != nil {
		if err := p.SettledPrivacy.validate(p); err != nil {
			return err
		}
	} else {
		if p.Commit.SettledPrivacySHA256 != "" || p.Commit.PreparationSHA256 != p.Preparation.SHA256 || p.Preparation.SHA256 != preparationSHAWithFacts(*p.Preparation, facts) {
			return ErrDurableStorageRecovery
		}
		if err := p.Preparation.validateWithFacts(facts); err != nil {
			return err
		}
		if p.Preparation.Migration != nil && p.Preparation.Migration.NextMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
			return ErrDurableStorageRecovery
		}
		if p.Commit.Purpose == PublicationPrivacyRewrite {
			if err := validatePrivacyCorrespondence(*p.Preparation, p.Payloads, p.PrivacyReceipts, p.MetadataBytes, privacyPreviousBody(*p.Preparation)); err != nil {
				return err
			}
		} else if len(p.PrivacyReceipts) > 0 {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

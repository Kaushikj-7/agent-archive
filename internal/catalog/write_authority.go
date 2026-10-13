package catalog

import "context"

type writeAuthorityKey struct{}

type writeAuthority struct {
	writer *Writer
	owner  string
	digest string
	seal   string
}

// Private helpers still check actual durable lifecycle or migration ownership;
// a private function name never grants permission through a sealed destination.
func (w *Writer) checkWriteAuthority(ctx context.Context) error {
	if w.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority, ok := ctx.Value(writeAuthorityKey{}).(writeAuthority)
	if !ok || authority.writer != w {
		return ErrAdmissionClosed
	}
	state, _, err := w.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Upgrade != nil {
		u, ok := ctx.Value(pickerUpgradeKey{}).(pickerUpgradeAuthority)
		if state.Upgrade.Phase == "draining" && authority.owner != "" {
			owner, exists := state.Owners[authority.owner]
			if exists && owner.Digest == authority.digest && state.Protocol == 10 && state.Hold == "" {
				return nil
			}
		}
		if !ok || u.writer != w || u.owner != state.Upgrade.Owner || state.Seal != u.owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		return nil
	}
	if state.Protocol == 11 {
		if err := w.writeFormat(ctx, state); err != nil {
			return err
		}
	}
	if state.Intent != nil {
		return ErrAdmissionClosed
	}
	if authority.seal != "" {
		if state.Seal != authority.seal || state.Hold != "" || len(state.Owners) != 0 {
			return ErrAdmissionClosed
		}
		return nil
	}
	owner, ok := state.Owners[authority.owner]
	if !ok || owner.Digest != authority.digest || state.Hold != "" {
		return ErrAdmissionClosed
	}
	return nil
}

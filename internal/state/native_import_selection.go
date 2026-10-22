package state

import "github.com/wangjohn/agent-archive/internal/archive"

func nativeImportSelectionAuthority(reg archive.SessionRegistration) (string, error) {
	if reg.NativeImport == nil {
		return "", nil
	}
	if err := reg.ValidateNativeImport(); err != nil {
		return "", err
	}
	return archive.NativeImportIdentityDigest(reg.NativeImport)
}

func validateNativeImportSelectionAuthority(reg archive.SessionRegistration, selection ReadySelection) error {
	expected, err := nativeImportSelectionAuthority(reg)
	if err != nil {
		return err
	}
	if selection.NativeImportAuthority != expected {
		return ErrReadySelectionFrozen
	}
	return nil
}

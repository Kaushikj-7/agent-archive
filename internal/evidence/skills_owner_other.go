//go:build !unix

package evidence

import "os"

// Native-local plugin ownership has not been qualified on this platform.
func skillEntryOwned(os.FileInfo) bool { return false }

func openConfinedSkillFile(*os.Root, string) (*os.File, error) { return nil, errSkillInventoryFile }

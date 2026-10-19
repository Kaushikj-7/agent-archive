package cli

import "fmt"

// lockHolder names what holds the collector lock, for a command that could
// not take it: the command, its process, and since when, from the record
// the holder writes (see recordCollectorLock), or a generic phrase when there
// is no record of a live holder (it was just released, or a status check
// holds it for an instant). Every command that finds the lock busy says who
// holds it this way.
func lockHolder(home string) string {
	record, ok := readCollectorLockRecord(home)
	if !ok || !processAlive(record.PID) {
		return "another agent-archive command"
	}
	name := "agent-archive " + record.Holder
	if record.Holder == "collect" {
		name = "the background collector"
	}
	return fmt.Sprintf("%s (process %d, since %s)", name, record.PID, record.Since.Local().Format("15:04:05"))
}

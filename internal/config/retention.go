package config

import (
	"fmt"
	"time"
)

// DefaultRetentionDays is the policy for new installations and absent legacy values.
const DefaultRetentionDays = 90

// MaxRetentionDays is the largest supported finite retention policy.
const MaxRetentionDays = 36500

// ValidateRetentionDays accepts zero (forever) or a finite policy.
func ValidateRetentionDays(days int) error {
	if days < 0 || days > MaxRetentionDays {
		return fmt.Errorf("retention_days must be between 0 (keep forever) and %d", MaxRetentionDays)
	}
	return nil
}

// RetentionShorter orders forever above every finite duration.
func RetentionShorter(next, previous int) bool {
	return next != 0 && (previous == 0 || next < previous)
}

// RetentionDuration converts validated days to an expiry age; zero disables expiry.
func RetentionDuration(days int) time.Duration { return time.Duration(days) * 24 * time.Hour }

// RetentionLabel renders validated days as a finite duration or Keep forever.
func RetentionLabel(days int) string {
	if days == 0 {
		return "Keep forever"
	}
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

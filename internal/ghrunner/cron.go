package ghrunner

import "raccoonci/internal/cron"

// Cron and ParseCron live in internal/cron, which the server shares.
type Cron = cron.Cron

// ParseCron parses a cron expression.
func ParseCron(s string) (*Cron, error) { return cron.Parse(s) }

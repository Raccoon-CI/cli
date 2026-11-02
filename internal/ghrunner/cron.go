package ghrunner

import "github.com/Raccoon-CI/raccoonci/pkg/cron"

// Cron and ParseCron live in pkg/cron, which the server shares.
type Cron = cron.Cron

// ParseCron parses a cron expression.
func ParseCron(s string) (*Cron, error) { return cron.Parse(s) }

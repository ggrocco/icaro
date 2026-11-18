package cli

import "icaro/internal/app"

// Agent steps' readiness (ADR 0037) is checked without a daemon too.
func init() { extraDoctorChecks = append(extraDoctorChecks, app.AgentStepsChecks) }

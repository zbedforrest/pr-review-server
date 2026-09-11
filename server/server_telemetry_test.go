package server

import "testing"

func TestAllowedTelemetryActionsIncludeTheDashboardControls(t *testing.T) {
	for _, action := range []string{"search", "open_pr_github", "status_count_panel"} {
		if !allowedActions[action] {
			t.Errorf("telemetry action %q is emitted by the dashboard but not accepted by the server", action)
		}
	}
}

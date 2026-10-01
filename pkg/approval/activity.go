package approval

import "context"

type Activity struct {
	Stage     string `json:"stage"`
	Tool      string `json:"tool,omitempty"`
	Round     int    `json:"round,omitempty"`
	ToolCalls int    `json:"tool_calls,omitempty"`
}

type activityObserverKey struct{}

func WithActivityObserver(ctx context.Context, observe func(Activity)) context.Context {
	return context.WithValue(ctx, activityObserverKey{}, observe)
}

func reportActivity(ctx context.Context, activity Activity) {
	if observe, ok := ctx.Value(activityObserverKey{}).(func(Activity)); ok {
		observe(activity)
	}
}

func ActivitySummary(a Activity) string {
	switch a.Stage {
	case "collecting":
		return "Gathering existing reviews and pull request evidence"
	case "repository":
		return "Preparing pinned code for inspection"
	case "model":
		return "Connecting review evidence with code findings"
	case "model_wait":
		return "Waiting for model capacity"
	case "citations":
		return "Checking citations against the reviewed code"
	case "validating":
		return "Rechecking current code and review evidence"
	case "tool":
		switch a.Tool {
		case "list_evidence":
			return "Mapping available reviews and discussion threads"
		case "read_evidence":
			return "Reading review findings and discussion threads"
		case "read_file":
			return "Inspecting code behind the review findings"
		case "search_code":
			return "Searching code for related behavior"
		case "list_files":
			return "Locating files relevant to review concerns"
		case "read_diff":
			return "Comparing code changes with earlier review concerns"
		}
	}
	return "Investigating existing reviews and supporting code"
}

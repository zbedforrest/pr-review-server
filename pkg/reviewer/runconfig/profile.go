package runconfig

import (
	"fmt"
	"strings"
)

// Review profiles name a complete pipeline shape. Full is the historical
// first-pass-plus-agent pipeline; lite and lite_plus are the single-agent
// flavors measured in the solo campaign (see PRism Lite spec).
const (
	ProfileFull     = "full"
	ProfileLite     = "lite"
	ProfileLitePlus = "lite_plus"

	PromptPipeline    = "pipeline"
	PromptLiteArmA    = "lite_arm_a"
	PromptLiteArmASub = "lite_arm_a_sub"

	ToolsDefault   = "Read,Grep,Glob,Bash"
	ToolsWithAgent = "Read,Grep,Glob,Bash,Agent"

	// LiteModel is the agent model both lite profiles pin; deployments admit
	// it regardless of their own agent model allowlist.
	LiteModel = "claude-fable-5-1"

	liteBackend              = "claude"
	liteModel                = LiteModel
	liteEffort               = "medium"
	liteWallClockSeconds     = 300
	liteMaxTurns             = 60
	litePlusWallClockSeconds = 600
	litePlusMaxTurns         = 120
)

var profileOrder = []string{ProfileFull, ProfileLite, ProfileLitePlus}

// Profiles lists every selectable profile name in display order.
func Profiles() []string {
	return append([]string(nil), profileOrder...)
}

// KnownProfile reports whether name is a selectable profile.
func KnownProfile(name string) bool {
	switch name {
	case ProfileFull, ProfileLite, ProfileLitePlus:
		return true
	}
	return false
}

// NormalizeProfile canonicalizes a caller-supplied profile name; "" is full.
func NormalizeProfile(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ProfileFull
	}
	return name
}

// ProfileLabel is the human name shown on dashboards and in the summary footer.
func ProfileLabel(profile string) string {
	switch profile {
	case ProfileLite:
		return "Lite"
	case ProfileLitePlus:
		return "Lite+"
	default:
		return "Full"
	}
}

// ProfileMaxWallClockSeconds and ProfileMaxTurns are the largest budgets any
// built-in profile needs; deployment ceilings must admit them.
func ProfileMaxWallClockSeconds() int { return litePlusWallClockSeconds }
func ProfileMaxTurns() int            { return litePlusMaxTurns }

// Expand returns the canonical effective config for profile. base is the
// deployment's full-pipeline default; full returns it with the profile fields
// made explicit, while lite and lite_plus replace every stage setting with the
// profile's fixed values regardless of deployment defaults.
func Expand(profile string, base Effective) (Effective, error) {
	profile = NormalizeProfile(profile)
	effective := base
	effective.SchemaVersion = SchemaVersion
	effective.Profile = profile
	switch profile {
	case ProfileFull:
		effective.Agent.Tools = ToolsDefault
		effective.Agent.Prompt = PromptPipeline
		effective.FirstPass.Enabled = true
		effective.Gates = true
		effective.BugMemory = true
	case ProfileLite, ProfileLitePlus:
		effective.Agent = Agent{
			Enabled: true, Backend: liteBackend, Model: liteModel, Effort: liteEffort,
			WallClockSeconds: liteWallClockSeconds, MaxTurns: liteMaxTurns,
			Tools: ToolsDefault, Prompt: PromptLiteArmA,
		}
		if profile == ProfileLitePlus {
			effective.Agent.WallClockSeconds = litePlusWallClockSeconds
			effective.Agent.MaxTurns = litePlusMaxTurns
			effective.Agent.Tools = ToolsWithAgent
			effective.Agent.Prompt = PromptLiteArmASub
		}
		effective.FirstPass = FirstPass{}
		effective.RequiredChecks = false
		effective.Gates = false
		effective.BugMemory = true
	default:
		return Effective{}, invalid("profile", "unknown profile %q (want full, lite, or lite_plus)", profile)
	}
	effective.Agent.TurnBudgetUnit, effective.Agent.TurnBudgetVersion = TurnBudgetSemantics(effective.Agent.Backend)
	return effective, nil
}

// ProfileDescription is the operator-facing account of which profile a run
// used and how, if at all, its effective config departs from that profile.
type ProfileDescription struct {
	// Profile is the canonical profile name; legacy configs report full.
	Profile string
	// Legacy is true for configs recorded before profiles existed.
	Legacy bool
	// Deviations lists each field that differs from the profile's canonical
	// expansion, e.g. "effort high (default medium)". Empty when exact.
	Deviations []string
}

// Custom reports whether the config deviates from its profile.
func (d ProfileDescription) Custom() bool { return len(d.Deviations) > 0 }

// Title renders the header line for reports: "Full", "Lite+",
// "Custom (based on Lite)" or "Full (legacy config)".
func (d ProfileDescription) Title() string {
	label := ProfileLabel(d.Profile)
	switch {
	case d.Legacy:
		return label + " (legacy config)"
	case d.Custom():
		return "Custom (based on " + label + ")"
	}
	return label
}

// Footer renders the summary-comment attribution: "" for an exact full run,
// otherwise "PRism Lite", "PRism Lite+ (custom)", "PRism Full (custom)".
func (d ProfileDescription) Footer() string {
	if d.Legacy || (d.Profile == ProfileFull && !d.Custom()) {
		return ""
	}
	footer := "PRism " + ProfileLabel(d.Profile)
	if d.Custom() {
		footer += " (custom)"
	}
	return footer
}

// DescribeProfile compares effective against its profile's canonical
// expansion. base is the deployment's full-pipeline default, which is the
// canonical expansion of the full profile; lite and lite_plus expansions are
// fixed and ignore it.
func DescribeProfile(effective Effective, base Effective) ProfileDescription {
	if effective.SchemaVersion < 4 || effective.Profile == "" {
		return ProfileDescription{Profile: ProfileFull, Legacy: true}
	}
	profile := NormalizeProfile(effective.Profile)
	canonical, err := Expand(profile, base)
	if err != nil {
		return ProfileDescription{Profile: ProfileFull, Legacy: true}
	}
	var deviations []string
	note := func(name, got, want string) {
		if got != want {
			deviations = append(deviations, fmt.Sprintf("%s %s (default %s)", name, got, want))
		}
	}
	note("backend", effective.Agent.Backend, canonical.Agent.Backend)
	note("model", effective.Agent.Model, canonical.Agent.Model)
	note("effort", effective.Agent.Effort, canonical.Agent.Effort)
	note("wall clock", fmt.Sprintf("%d s", effective.Agent.WallClockSeconds), fmt.Sprintf("%d s", canonical.Agent.WallClockSeconds))
	note("max turns", fmt.Sprint(effective.Agent.MaxTurns), fmt.Sprint(canonical.Agent.MaxTurns))
	note("tools", effective.Agent.Tools, canonical.Agent.Tools)
	note("prompt", effective.Agent.Prompt, canonical.Agent.Prompt)
	note("agent", onOff(effective.Agent.Enabled), onOff(canonical.Agent.Enabled))
	note("first pass", onOff(effective.FirstPass.Enabled), onOff(canonical.FirstPass.Enabled))
	if effective.FirstPass.Enabled && canonical.FirstPass.Enabled {
		note("first-pass samples", fmt.Sprint(effective.FirstPass.Samples), fmt.Sprint(canonical.FirstPass.Samples))
		note("first-pass provider", effective.FirstPass.Provider, canonical.FirstPass.Provider)
		note("first-pass model", effective.FirstPass.Model, canonical.FirstPass.Model)
	}
	note("required checks", onOff(effective.RequiredChecks), onOff(canonical.RequiredChecks))
	note("gates", onOff(effective.Gates), onOff(canonical.Gates))
	note("bug memory", onOff(effective.BugMemory), onOff(canonical.BugMemory))
	return ProfileDescription{Profile: profile, Deviations: deviations}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// ValidTools reports whether a tools allowlist is one the pipeline supports.
func ValidTools(tools string) bool {
	if tools == "" {
		return false
	}
	seen := map[string]bool{}
	for _, tool := range strings.Split(tools, ",") {
		tool = strings.TrimSpace(tool)
		switch tool {
		case "Read", "Grep", "Glob", "Bash", "Agent":
		default:
			return false
		}
		if seen[tool] {
			return false
		}
		seen[tool] = true
	}
	for _, required := range []string{"Read", "Grep", "Glob", "Bash"} {
		if !seen[required] {
			return false
		}
	}
	return true
}

func validPrompt(prompt string) bool {
	switch prompt {
	case PromptPipeline, PromptLiteArmA, PromptLiteArmASub:
		return true
	}
	return false
}

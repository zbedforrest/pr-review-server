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
	// PromptLiteArmAV2 is the measured Arm A text with a compact output
	// contract: the lite default since the 2026-09-18 iteration. V2Sub adds
	// the sub-agent sentence for lite_plus; V3 is V2 with bug memory, kept
	// selectable as the measured control. lite_arm_a and lite_arm_a_sub stay
	// selectable as the legacy shapes.
	PromptLiteArmAV2    = "lite_arm_a_v2"
	PromptLiteArmAV2Sub = "lite_arm_a_v2_sub"
	PromptLiteArmAV3    = "lite_arm_a_v3"

	ToolsDefault   = "Read,Grep,Glob,Bash"
	ToolsWithAgent = "Read,Grep,Glob,Bash,Agent"

	// LiteModel is the agent model both lite profiles pin; deployments admit
	// it regardless of their own agent model allowlist.
	LiteModel = "claude-fable-5-1"

	liteBackend          = "claude"
	liteModel            = LiteModel
	liteEffort           = "medium"
	liteWallClockSeconds = 300
	liteMaxTurns         = 60
	// liteCappedDiffWallClockSeconds replaces liteWallClockSeconds at run time
	// when the inlined diff hit the 60k-char cap: both wall-clock kills in the
	// v2 validation were capped diffs. It is part of the lite default, so it
	// never counts as a deviation.
	liteCappedDiffWallClockSeconds = 360
	litePlusWallClockSeconds       = 600
	litePlusMaxTurns               = 120
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

// CappedDiffWallClockSeconds is the agent wall clock a run gets when its
// inlined diff was cut at the cap: the lite profile's 360 s, but only while
// the run still carries the profile's own 300 s (a caller-chosen wall clock
// stands as given). 0 means no change for every other profile.
func CappedDiffWallClockSeconds(effective Effective) int {
	if NormalizeProfile(effective.Profile) == ProfileLite && effective.Agent.WallClockSeconds == liteWallClockSeconds {
		return liteCappedDiffWallClockSeconds
	}
	return 0
}

// ProfileNote is the operator-facing footnote to a profile's effective config,
// for behavior the fixed fields cannot show; "" when there is none.
func ProfileNote(profile string) string {
	if NormalizeProfile(profile) == ProfileLite {
		return fmt.Sprintf("agent.wall_clock_seconds becomes %d when the inlined diff is cut at the 60000-character cap; this is the profile default, not a deviation",
			liteCappedDiffWallClockSeconds)
	}
	return ""
}

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
			Tools: ToolsDefault, Prompt: PromptLiteArmAV2,
		}
		if profile == ProfileLitePlus {
			effective.Agent.WallClockSeconds = litePlusWallClockSeconds
			effective.Agent.MaxTurns = litePlusMaxTurns
			effective.Agent.Tools = ToolsWithAgent
			effective.Agent.Prompt = PromptLiteArmAV2Sub
		}
		effective.FirstPass = FirstPass{}
		effective.RequiredChecks = false
		effective.Gates = false
		// Bug memory cost this agent 6.0 of 37 battery cases (lite_arm_a_v3
		// vs v2); the full pipeline keeps it.
		effective.BugMemory = false
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
	return prompt == PromptPipeline || LitePrompt(prompt)
}

// LitePrompt reports whether prompt is one of the single-agent shapes that
// inline the diff and run without gates or required checks.
func LitePrompt(prompt string) bool {
	switch prompt {
	case PromptLiteArmA, PromptLiteArmASub, PromptLiteArmAV2, PromptLiteArmAV2Sub, PromptLiteArmAV3:
		return true
	}
	return false
}

// PromptUsesBugMemory reports whether a lite prompt renders the bug-history
// section; the lite profiles turn bug memory on only for those shapes, so the
// v3 control and the legacy prompts keep the library they were measured with.
func PromptUsesBugMemory(prompt string) bool {
	switch prompt {
	case PromptLiteArmA, PromptLiteArmASub, PromptLiteArmAV3:
		return true
	}
	return false
}

// promptFitsProfile keeps the prompt shape inside its pipeline: the full
// profile builds around first-pass claims, the lite profiles around an
// inlined diff, and neither prompt makes sense in the other pipeline.
func promptFitsProfile(profile, prompt string) bool {
	if NormalizeProfile(profile) == ProfileFull {
		return prompt == PromptPipeline
	}
	return LitePrompt(prompt)
}

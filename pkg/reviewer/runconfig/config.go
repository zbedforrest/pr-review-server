// Package runconfig defines the caller-customizable review configuration.
//
// HTTP requests carry Overrides, while the executor receives only Effective:
// a complete, validated snapshot whose values cannot change underneath a
// queued run when deployment defaults change.
package runconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const SchemaVersion = 3

const (
	SourceRequest           = "request"
	SourceDeploymentDefault = "deployment_default"
	SourceDerived           = "derived"

	TurnBudgetVersion                       = 1
	TurnBudgetUnitAssistantEvent            = "assistant_event"
	TurnBudgetUnitCompletedNonReasoningItem = "completed_non_reasoning_item"

	BackendUnavailablePolicyDisabled    = "policy_disabled"
	BackendUnavailableCredentialMissing = "credential_missing"
	BackendUnavailableGitMissing        = "git_executable_missing"
	BackendUnavailableCLIMissing        = "backend_executable_missing"
)

// Overrides is the public, optional configuration accepted from a caller.
// Pointer fields distinguish omission from explicit false/zero values, which
// lets validation reject invalid input instead of silently defaulting it.
type Overrides struct {
	Agent          *AgentOverrides     `json:"agent,omitempty"`
	FirstPass      *FirstPassOverrides `json:"first_pass,omitempty"`
	RequiredChecks *bool               `json:"required_checks,omitempty"`
}

type AgentOverrides struct {
	Enabled          *bool   `json:"enabled,omitempty"`
	Backend          *string `json:"backend,omitempty"`
	Model            *string `json:"model,omitempty"`
	Effort           *string `json:"effort,omitempty"`
	WallClockSeconds *int    `json:"wall_clock_seconds,omitempty"`
	MaxTurns         *int    `json:"max_turns,omitempty"`
}

type FirstPassOverrides struct {
	Samples  *int    `json:"samples,omitempty"`
	Provider *string `json:"provider,omitempty"`
	Model    *string `json:"model,omitempty"`
}

// Effective is the complete execution configuration persisted before work
// begins and passed unchanged through the review pipeline.
type Effective struct {
	SchemaVersion  int       `json:"schema_version"`
	Agent          Agent     `json:"agent"`
	FirstPass      FirstPass `json:"first_pass"`
	RequiredChecks bool      `json:"required_checks"`
}

type Agent struct {
	Enabled           bool   `json:"enabled"`
	Backend           string `json:"backend"`
	Model             string `json:"model"`
	Effort            string `json:"effort"`
	WallClockSeconds  int    `json:"wall_clock_seconds"`
	MaxTurns          int    `json:"max_turns"`
	TurnBudgetUnit    string `json:"turn_budget_unit"`
	TurnBudgetVersion int    `json:"turn_budget_version"`
}

type FirstPass struct {
	Samples  int    `json:"samples"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// BackendPolicy describes one deployment-enabled agent backend. Models and
// Efforts are allowlists; an empty list permits no caller-visible values.
type BackendPolicy struct {
	// Available retains the legacy runnable-readiness signal. Ready is an
	// explicit alias for newer clients; producers intentionally keep them equal.
	// PolicyEnabled separately exposes deployment policy admission.
	// The readiness fields explain failures without exposing credential values
	// or executable paths.
	Available            bool
	Ready                bool
	PolicyEnabled        bool
	CredentialConfigured bool
	CredentialRequired   bool
	ExecutableAvailable  bool
	UnavailableReasons   []string
	TurnBudgetUnit       string
	TurnBudgetVersion    int
	DefaultMaxTurns      int
	MaxTurns             int
	Models               []string
	Efforts              []string
}

// FirstPassProviderPolicy describes one deployment-enabled first-pass
// provider. Models is an allowlist; DefaultModel resolves a provider switch
// whose caller omitted an explicit model.
type FirstPassProviderPolicy struct {
	CredentialConfigured bool
	DefaultModel         string
	Models               []string
}

// Policy contains operator-owned safety limits. Callers can choose within
// these bounds but cannot increase process concurrency or supply credentials.
type Policy struct {
	Backends            map[string]BackendPolicy
	FirstPassProviders  map[string]FirstPassProviderPolicy
	MaxWallClockSeconds int
	MaxTurns            int
	MaxFirstPassSamples int
}

// Snapshot is the safe, durable configuration metadata attached to a run.
// Sources is keyed by JSON-style field path, for example "agent.model".
type Snapshot struct {
	Requested Overrides         `json:"requested"`
	Effective Effective         `json:"effective"`
	Sources   map[string]string `json:"sources"`
	Hash      string            `json:"hash"`
}

// ValidationError identifies the exact public field that failed validation.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Resolve applies caller overrides to defaults, validates the complete
// result against deployment policy, and returns a deterministic snapshot.
func Resolve(requested Overrides, defaults Effective, policy Policy) (Snapshot, error) {
	effective := defaults
	effective.SchemaVersion = SchemaVersion
	sources := defaultSources()

	if requested.Agent != nil {
		a := requested.Agent
		if a.Enabled != nil {
			effective.Agent.Enabled = *a.Enabled
			sources["agent.enabled"] = SourceRequest
		}
		if a.Backend != nil {
			effective.Agent.Backend = *a.Backend
			sources["agent.backend"] = SourceRequest
		}
		if a.Model != nil {
			effective.Agent.Model = strings.TrimSpace(*a.Model)
			sources["agent.model"] = SourceRequest
		}
		if a.Effort != nil {
			effective.Agent.Effort = *a.Effort
			sources["agent.effort"] = SourceRequest
		}
		if a.WallClockSeconds != nil {
			effective.Agent.WallClockSeconds = *a.WallClockSeconds
			sources["agent.wall_clock_seconds"] = SourceRequest
		}
		if a.MaxTurns != nil {
			effective.Agent.MaxTurns = *a.MaxTurns
			sources["agent.max_turns"] = SourceRequest
		}
	}
	if requested.FirstPass != nil {
		f := requested.FirstPass
		if f.Samples != nil {
			effective.FirstPass.Samples = *f.Samples
			sources["first_pass.samples"] = SourceRequest
		}
		if f.Provider != nil {
			effective.FirstPass.Provider = *f.Provider
			sources["first_pass.provider"] = SourceRequest
		}
		if f.Model != nil {
			effective.FirstPass.Model = strings.TrimSpace(*f.Model)
			sources["first_pass.model"] = SourceRequest
		}
	}
	if requested.RequiredChecks != nil {
		effective.RequiredChecks = *requested.RequiredChecks
		sources["required_checks"] = SourceRequest
	}

	// Canonicalize regardless of source so durable snapshots and hashes remain
	// stable when deployment defaults use non-canonical casing or whitespace.
	effective.Agent.Backend = strings.ToLower(strings.TrimSpace(effective.Agent.Backend))
	effective.Agent.Model = strings.TrimSpace(effective.Agent.Model)
	effective.Agent.Effort = strings.ToLower(strings.TrimSpace(effective.Agent.Effort))
	effective.FirstPass.Provider = strings.ToLower(strings.TrimSpace(effective.FirstPass.Provider))
	effective.FirstPass.Model = strings.TrimSpace(effective.FirstPass.Model)
	effective.Agent.TurnBudgetUnit, effective.Agent.TurnBudgetVersion = TurnBudgetSemantics(effective.Agent.Backend)
	// A provider switch moves model ids into another provider's namespace. When
	// the caller omitted model, derive that provider's default instead of
	// carrying the deployment provider's model.
	if sources["first_pass.model"] != SourceRequest &&
		effective.FirstPass.Provider != strings.ToLower(strings.TrimSpace(defaults.FirstPass.Provider)) {
		if providerPolicy, ok := policy.FirstPassProviders[effective.FirstPass.Provider]; ok {
			if providerPolicy.DefaultModel == "" {
				return Snapshot{}, invalid("first_pass.model", "provider %q has no default model; specify model", effective.FirstPass.Provider)
			}
			effective.FirstPass.Model = providerPolicy.DefaultModel
			sources["first_pass.model"] = SourceDerived
		}
	}
	// A backend switch changes the unit represented by max_turns. When the
	// caller omitted max_turns, derive that backend's default instead of
	// carrying a number expressed in the deployment backend's unit.
	if sources["agent.max_turns"] != SourceRequest &&
		effective.Agent.TurnBudgetUnit != defaults.Agent.TurnBudgetUnit {
		if backendPolicy, ok := findBackendPolicy(policy, effective.Agent.Backend); ok {
			if backendPolicy.DefaultMaxTurns <= 0 {
				return Snapshot{}, invalid("agent.max_turns", "backend %q has no default turn budget; specify max_turns", effective.Agent.Backend)
			}
			effective.Agent.MaxTurns = backendPolicy.DefaultMaxTurns
			if backendPolicy.MaxTurns > 0 && effective.Agent.MaxTurns > backendPolicy.MaxTurns {
				effective.Agent.MaxTurns = backendPolicy.MaxTurns
			}
			sources["agent.max_turns"] = SourceDerived
		}
	}

	if err := Validate(effective, policy); err != nil {
		return Snapshot{}, err
	}
	hash, err := Hash(effective)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Requested: requested,
		Effective: effective,
		Sources:   sources,
		Hash:      hash,
	}, nil
}

func Validate(cfg Effective, policy Policy) error {
	if cfg.SchemaVersion != SchemaVersion {
		return invalid("schema_version", "unsupported value %d", cfg.SchemaVersion)
	}
	if cfg.FirstPass.Samples <= 0 {
		return invalid("first_pass.samples", "must be greater than zero")
	}
	if policy.MaxFirstPassSamples > 0 && cfg.FirstPass.Samples > policy.MaxFirstPassSamples {
		return invalid("first_pass.samples", "must be at most %d", policy.MaxFirstPassSamples)
	}
	if err := validateFirstPassIdentity(cfg.FirstPass, policy); err != nil {
		return err
	}
	if !cfg.Agent.Enabled {
		return nil
	}

	backend := strings.ToLower(strings.TrimSpace(cfg.Agent.Backend))
	backendPolicy, ok := findBackendPolicy(policy, backend)
	if !ok {
		return invalid("agent.backend", "unsupported backend %q", cfg.Agent.Backend)
	}
	if !backendPolicy.Available || !backendPolicy.Ready {
		return invalid("agent.backend", "backend %q is not ready in this deployment", backend)
	}
	if cfg.Agent.TurnBudgetUnit != backendPolicy.TurnBudgetUnit || cfg.Agent.TurnBudgetVersion != backendPolicy.TurnBudgetVersion {
		return invalid("agent.turn_budget_unit", "turn-budget semantics do not match backend %q", backend)
	}
	// Provider model IDs are case-sensitive; unlike backend and effort, model
	// matching is deliberately exact.
	if !contains(backendPolicy.Models, cfg.Agent.Model) {
		return invalid("agent.model", "model %q is not allowed for backend %q", cfg.Agent.Model, backend)
	}
	if !containsFold(backendPolicy.Efforts, cfg.Agent.Effort) {
		return invalid("agent.effort", "effort %q is not supported for backend %q", cfg.Agent.Effort, backend)
	}
	if cfg.Agent.WallClockSeconds <= 0 {
		return invalid("agent.wall_clock_seconds", "must be greater than zero")
	}
	if policy.MaxWallClockSeconds > 0 && cfg.Agent.WallClockSeconds > policy.MaxWallClockSeconds {
		return invalid("agent.wall_clock_seconds", "must be at most %d", policy.MaxWallClockSeconds)
	}
	if cfg.Agent.MaxTurns <= 0 {
		return invalid("agent.max_turns", "must be greater than zero")
	}
	maxTurns := backendPolicy.MaxTurns
	if maxTurns <= 0 {
		// Legacy/hand-built single-backend policies predate per-backend units;
		// their one global ceiling is necessarily expressed in that backend's
		// unit. A multi-backend policy cannot safely make that assumption.
		if len(policy.Backends) == 1 {
			maxTurns = policy.MaxTurns
		} else {
			return invalid("agent.max_turns", "backend %q has no configured turn ceiling in this deployment", backend)
		}
	}
	if maxTurns > 0 && cfg.Agent.MaxTurns > maxTurns {
		return invalid("agent.max_turns", "must be at most %d", maxTurns)
	}
	return nil
}

// validateFirstPassIdentity checks a resolved first-pass provider/model pair
// against the deployment's provider policies. A fully empty identity with no
// declared providers is a pre-provider-selection legacy config and passes.
func validateFirstPassIdentity(firstPass FirstPass, policy Policy) error {
	if firstPass.Provider == "" && firstPass.Model == "" && len(policy.FirstPassProviders) == 0 {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(firstPass.Provider))
	providerPolicy, ok := policy.FirstPassProviders[provider]
	if !ok {
		return invalid("first_pass.provider", "unsupported provider %q", firstPass.Provider)
	}
	if !providerPolicy.CredentialConfigured {
		return invalid("first_pass.provider", "provider %q has no credential configured in this deployment", provider)
	}
	// Provider model IDs are case-sensitive; matching is deliberately exact.
	if !contains(providerPolicy.Models, firstPass.Model) {
		return invalid("first_pass.model", "model %q is not allowed for provider %q", firstPass.Model, provider)
	}
	return nil
}

func findBackendPolicy(policy Policy, backend string) (BackendPolicy, bool) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if candidate, ok := policy.Backends[backend]; ok {
		return candidate, true
	}
	for name, candidate := range policy.Backends {
		if strings.EqualFold(strings.TrimSpace(name), backend) {
			return candidate, true
		}
	}
	return BackendPolicy{}, false
}

// TurnBudgetSemantics returns the immutable counter contract for a backend.
// MaxTurns keeps its public name for compatibility, while these fields make
// the unit precise in every durable effective-config snapshot.
func TurnBudgetSemantics(backend string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "claude":
		return TurnBudgetUnitAssistantEvent, TurnBudgetVersion
	case "openrouter":
		return TurnBudgetUnitCompletedNonReasoningItem, TurnBudgetVersion
	default:
		return "", 0
	}
}

// Hash returns a stable SHA-256 of the complete effective configuration.
func Hash(cfg Effective) (string, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal effective review config: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// AvailableBackends returns stable, sorted backend names for capability APIs.
func (p Policy) AvailableBackends() []string {
	available := make(map[string]struct{}, len(p.Backends))
	for name, backend := range p.Backends {
		if backend.Available && backend.Ready {
			available[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
		}
	}
	backends := make([]string, 0, len(available))
	for name := range available {
		backends = append(backends, name)
	}
	sort.Strings(backends)
	return backends
}

func defaultSources() map[string]string {
	return map[string]string{
		"agent.enabled":             SourceDeploymentDefault,
		"agent.backend":             SourceDeploymentDefault,
		"agent.model":               SourceDeploymentDefault,
		"agent.effort":              SourceDeploymentDefault,
		"agent.wall_clock_seconds":  SourceDeploymentDefault,
		"agent.max_turns":           SourceDeploymentDefault,
		"agent.turn_budget_unit":    SourceDerived,
		"agent.turn_budget_version": SourceDerived,
		"first_pass.samples":        SourceDeploymentDefault,
		"first_pass.provider":       SourceDeploymentDefault,
		"first_pass.model":          SourceDeploymentDefault,
		"required_checks":           SourceDeploymentDefault,
	}
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

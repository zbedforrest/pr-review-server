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

const SchemaVersion = 1

const (
	SourceRequest           = "request"
	SourceDeploymentDefault = "deployment_default"
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
	Samples *int `json:"samples,omitempty"`
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
	Enabled          bool   `json:"enabled"`
	Backend          string `json:"backend"`
	Model            string `json:"model"`
	Effort           string `json:"effort"`
	WallClockSeconds int    `json:"wall_clock_seconds"`
	MaxTurns         int    `json:"max_turns"`
}

type FirstPass struct {
	Samples int `json:"samples"`
}

// BackendPolicy describes one deployment-enabled agent backend. Models and
// Efforts are allowlists; an empty list permits no caller-visible values.
type BackendPolicy struct {
	Available bool
	Models    []string
	Efforts   []string
}

// Policy contains operator-owned safety limits. Callers can choose within
// these bounds but cannot increase process concurrency or supply credentials.
type Policy struct {
	Backends            map[string]BackendPolicy
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
	if requested.FirstPass != nil && requested.FirstPass.Samples != nil {
		effective.FirstPass.Samples = *requested.FirstPass.Samples
		sources["first_pass.samples"] = SourceRequest
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
	if !cfg.Agent.Enabled {
		return nil
	}

	backend := strings.ToLower(strings.TrimSpace(cfg.Agent.Backend))
	backendPolicy, ok := policy.Backends[backend]
	if !ok {
		return invalid("agent.backend", "unsupported backend %q", cfg.Agent.Backend)
	}
	if !backendPolicy.Available {
		return invalid("agent.backend", "backend %q is not available in this deployment", backend)
	}
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
	if policy.MaxTurns > 0 && cfg.Agent.MaxTurns > policy.MaxTurns {
		return invalid("agent.max_turns", "must be at most %d", policy.MaxTurns)
	}
	return nil
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
	backends := make([]string, 0, len(p.Backends))
	for name, backend := range p.Backends {
		if backend.Available {
			backends = append(backends, name)
		}
	}
	sort.Strings(backends)
	return backends
}

func defaultSources() map[string]string {
	return map[string]string{
		"agent.enabled":            SourceDeploymentDefault,
		"agent.backend":            SourceDeploymentDefault,
		"agent.model":              SourceDeploymentDefault,
		"agent.effort":             SourceDeploymentDefault,
		"agent.wall_clock_seconds": SourceDeploymentDefault,
		"agent.max_turns":          SourceDeploymentDefault,
		"first_pass.samples":       SourceDeploymentDefault,
		"required_checks":          SourceDeploymentDefault,
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

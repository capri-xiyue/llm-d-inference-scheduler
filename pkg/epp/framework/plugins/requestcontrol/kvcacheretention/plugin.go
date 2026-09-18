/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package kvcacheretention provides a PreRequest plugin that annotates
// requests from multi-turn sessions with KV-cache retention directives for
// backends implementing the vLLM Context-Aware KV-Cache Retention API
// (https://github.com/vllm-project/vllm/issues/37003).
//
// The plugin learns the log-normal distribution of a workload's inter-turn
// intervals online and sets each request's retention duration to a configured
// quantile of the fitted distribution: the session's KV blocks stay protected
// while the next turn is likely to arrive and fall back to LRU once the
// session is far into the distribution's tail. Session identity and workload
// type come from request headers supplied by the orchestrator.
package kvcacheretention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

const (
	// PluginType is the plugin type registered with the framework.
	PluginType = "kv-cache-retention"

	// retentionDirectivesField and retentionScopeField are the request-body
	// fields of the vLLM KV-Cache Retention API.
	retentionDirectivesField = "retention_directives"
	retentionScopeField      = "retention_scope"
)

var (
	_ requestcontrol.PreRequest            = &Plugin{}
	_ requestcontrol.ResponseBodyProcessor = &Plugin{}
	_ fwkplugin.StateDumper                = &Plugin{}
)

// Config holds the plugin parameters. Durations are time.ParseDuration
// strings, e.g. "30s". See README.md for what each one does.
type Config struct {
	// SessionIDHeader carries the orchestrator-assigned session identifier.
	SessionIDHeader string `json:"sessionIDHeader,omitempty"`
	// SessionTypeHeader carries the orchestrator-assigned workload type.
	SessionTypeHeader string `json:"sessionTypeHeader,omitempty"`
	// SessionType is the workload type this plugin acts on; requests with any
	// other value pass through untouched. Empty matches every request that
	// carries a session identifier.
	SessionType string `json:"sessionType,omitempty"`

	// Priority is the eviction priority (0-100) written into the directive.
	Priority int `json:"priority,omitempty"`
	// Quantile of the fitted inter-turn distribution used as the retention
	// duration, in (0, 1).
	Quantile float64 `json:"quantile,omitempty"`
	// MinRetention and MaxRetention clamp the computed duration.
	MinRetention string `json:"minRetention,omitempty"`
	MaxRetention string `json:"maxRetention,omitempty"`

	// InitialLogMean and InitialLogStd seed the estimator, in log-seconds.
	// The defaults are the CC-Bench agentic-trace fit reported in the
	// SAECache paper (arXiv:2605.18825, mu=2.28, sigma=1.34).
	InitialLogMean float64 `json:"initialLogMean,omitempty"`
	InitialLogStd  float64 `json:"initialLogStd,omitempty"`
	// EMAFactor is the blend weight of each new sample estimate, in (0, 1].
	EMAFactor float64 `json:"emaFactor,omitempty"`
	// MinSamples gates estimator updates until the window holds this many
	// observations.
	MinSamples int `json:"minSamples,omitempty"`
	// WindowSize is the sliding-window capacity of the sample estimate.
	WindowSize int `json:"windowSize,omitempty"`

	// MinInterval discards gap observations below the timestamp-precision
	// floor (tool-result auto-fills arriving effectively instantly).
	MinInterval string `json:"minInterval,omitempty"`
	// MaxIdle bounds a usable gap observation and the session sweep horizon.
	MaxIdle string `json:"maxIdle,omitempty"`
	// MaxSessions is the soft cap on tracked sessions.
	MaxSessions int `json:"maxSessions,omitempty"`
}

// DefaultConfig is decoded over by the factory.
var DefaultConfig = Config{
	SessionIDHeader:   "x-session-id",
	SessionTypeHeader: "x-session-type",
	SessionType:       "agentic",
	Priority:          70,
	Quantile:          0.9,
	MinRetention:      "1s",
	MaxRetention:      "10m",
	InitialLogMean:    2.28,
	InitialLogStd:     1.34,
	EMAFactor:         0.1,
	MinSamples:        20,
	WindowSize:        200,
	MinInterval:       "100ms",
	MaxIdle:           "1h",
	MaxSessions:       100000,
}

// resolvedConfig is Config after parsing and validation.
type resolvedConfig struct {
	sessionIDHeader   string
	sessionTypeHeader string
	sessionType       string
	priority          int
	quantile          float64
	minRetention      time.Duration
	maxRetention      time.Duration
	initialLogMean    float64
	initialLogStd     float64
	emaFactor         float64
	minSamples        int
	windowSize        int
	minInterval       time.Duration
	maxIdle           time.Duration
	maxSessions       int
}

func (c Config) resolve() (resolvedConfig, error) {
	var out resolvedConfig
	var err error

	if out.minRetention, err = positiveDuration("minRetention", c.MinRetention); err != nil {
		return out, err
	}
	if out.maxRetention, err = positiveDuration("maxRetention", c.MaxRetention); err != nil {
		return out, err
	}
	if out.minInterval, err = positiveDuration("minInterval", c.MinInterval); err != nil {
		return out, err
	}
	if out.maxIdle, err = positiveDuration("maxIdle", c.MaxIdle); err != nil {
		return out, err
	}

	switch {
	case strings.TrimSpace(c.SessionIDHeader) == "":
		return out, errors.New("sessionIDHeader must not be empty")
	case c.Priority < 0 || c.Priority > 100:
		return out, fmt.Errorf("priority must be in [0, 100], got %d", c.Priority)
	case c.Quantile <= 0 || c.Quantile >= 1:
		return out, fmt.Errorf("quantile must be in (0, 1), got %v", c.Quantile)
	case out.minRetention > out.maxRetention:
		return out, fmt.Errorf("minRetention (%v) must be <= maxRetention (%v)", out.minRetention, out.maxRetention)
	case c.InitialLogStd <= 0:
		return out, fmt.Errorf("initialLogStd must be > 0, got %v", c.InitialLogStd)
	case c.EMAFactor <= 0 || c.EMAFactor > 1:
		return out, fmt.Errorf("emaFactor must be in (0, 1], got %v", c.EMAFactor)
	case c.MinSamples < 2:
		return out, fmt.Errorf("minSamples must be >= 2, got %d", c.MinSamples)
	case c.WindowSize < c.MinSamples:
		return out, fmt.Errorf("windowSize (%d) must be >= minSamples (%d)", c.WindowSize, c.MinSamples)
	case c.MaxSessions <= 0:
		return out, fmt.Errorf("maxSessions must be > 0, got %d", c.MaxSessions)
	}

	// Request headers are stored with lowercased keys (see
	// handlers.HandleRequestHeaders), so the configured names must match.
	out.sessionIDHeader = strings.ToLower(strings.TrimSpace(c.SessionIDHeader))
	out.sessionTypeHeader = strings.ToLower(strings.TrimSpace(c.SessionTypeHeader))
	out.sessionType = strings.TrimSpace(c.SessionType)
	out.priority = c.Priority
	out.quantile = c.Quantile
	out.initialLogMean = c.InitialLogMean
	out.initialLogStd = c.InitialLogStd
	out.emaFactor = c.EMAFactor
	out.minSamples = c.MinSamples
	out.windowSize = c.WindowSize
	out.maxSessions = c.MaxSessions
	return out, nil
}

func positiveDuration(field, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %v", field, d)
	}
	return d, nil
}

// Plugin annotates session requests with retention directives. It is a
// process-lifetime singleton serving every request, so all shared state is
// guarded.
type Plugin struct {
	typedName fwkplugin.TypedName
	cfg       resolvedConfig
	estimator *logNormalEstimator
	tracker   *sessionTracker
	now       func() time.Time
}

// Factory builds a Plugin from raw plugin parameters.
func Factory(name string, rawParameters *json.Decoder, _ fwkplugin.Handle) (fwkplugin.Plugin, error) {
	cfg := DefaultConfig
	if rawParameters != nil {
		if err := rawParameters.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("failed to parse parameters for plugin %q: %w", name, err)
		}
	}
	plugin, err := NewPlugin(name, cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid parameters for plugin %q: %w", name, err)
	}
	return plugin, nil
}

// NewPlugin initializes a Plugin from a validated Config.
func NewPlugin(name string, cfg Config) (*Plugin, error) {
	resolved, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	return &Plugin{
		typedName: fwkplugin.TypedName{Type: PluginType, Name: name},
		cfg:       resolved,
		estimator: newLogNormalEstimator(resolved.initialLogMean, resolved.initialLogStd,
			resolved.emaFactor, resolved.minSamples, resolved.windowSize),
		tracker: newSessionTracker(resolved.maxSessions, resolved.maxIdle),
		now:     time.Now,
	}, nil
}

// TypedName returns the type and name of the plugin.
func (p *Plugin) TypedName() fwkplugin.TypedName { return p.typedName }

// PreRequest feeds the session's idle gap to the estimator and writes the
// retention directive into the request body.
//
// Always returns nil: a returned error fails the request, and failing to
// annotate retention is never a reason to reject one.
func (p *Plugin) PreRequest(ctx context.Context, request *fwksched.InferenceRequest, _ *fwksched.SchedulingResult) error {
	sessionID := p.sessionID(request)
	if sessionID == "" {
		return nil
	}
	now := p.now()
	if gap, ok := p.tracker.observe(sessionID, now); ok && gap >= p.cfg.minInterval {
		p.estimator.observe(gap.Seconds())
	}

	if request.Body == nil || request.Body.Payload == nil {
		return nil
	}
	payload, ok := request.Body.Payload.AsMap()
	if !ok {
		return nil
	}
	if _, exists := payload[retentionDirectivesField]; exists {
		// Client-supplied directives win.
		return nil
	}

	duration := p.retentionDuration()
	request.Body.MutatePayloadMap(func(m fwkrh.PayloadMap) {
		m[retentionDirectivesField] = []any{map[string]any{
			"start":    0,
			"end":      nil,
			"priority": p.cfg.priority,
			"duration": duration.Seconds(),
		}}
		m[retentionScopeField] = sessionID
	})

	if debugLogger := log.FromContext(ctx).V(logutil.DEBUG); debugLogger.Enabled() {
		logMean, logStd, observed := p.estimator.snapshot()
		debugLogger.Info("kv-cache-retention directive set", "sessionID", sessionID,
			"durationSeconds", duration.Seconds(), "priority", p.cfg.priority,
			"logMean", logMean, "logStd", logStd, "observations", observed)
	}
	return nil
}

// ResponseBody records the session's activity when the response completes, so
// the next turn's gap measures idle time rather than turnaround time.
func (p *Plugin) ResponseBody(_ context.Context, request *fwksched.InferenceRequest, response *requestcontrol.Response, _ *fwkdl.EndpointMetadata) {
	if response == nil || !response.EndOfStream {
		return
	}
	sessionID := p.sessionID(request)
	if sessionID == "" {
		return
	}
	p.tracker.touch(sessionID, p.now())
}

// retentionDuration returns the configured quantile of the fitted
// distribution, clamped to the configured bounds. Clamping happens on the
// float value so an extreme quantile never overflows the Duration conversion.
func (p *Plugin) retentionDuration() time.Duration {
	seconds := p.estimator.quantile(p.cfg.quantile)
	if math.IsNaN(seconds) || seconds < p.cfg.minRetention.Seconds() {
		return p.cfg.minRetention
	}
	if seconds > p.cfg.maxRetention.Seconds() {
		return p.cfg.maxRetention
	}
	return time.Duration(seconds * float64(time.Second))
}

// sessionID returns the request's session identifier, or empty when the
// request carries none or its workload type does not match.
func (p *Plugin) sessionID(request *fwksched.InferenceRequest) string {
	if request == nil || request.Headers == nil {
		return ""
	}
	if p.cfg.sessionType != "" &&
		!strings.EqualFold(strings.TrimSpace(request.Headers[p.cfg.sessionTypeHeader]), p.cfg.sessionType) {
		return ""
	}
	return strings.TrimSpace(request.Headers[p.cfg.sessionIDHeader])
}

type debugState struct {
	LogMean          float64 `json:"logMean"`
	LogStd           float64 `json:"logStd"`
	Observations     int64   `json:"observations"`
	RetentionSeconds float64 `json:"retentionSeconds"`
	TrackedSessions  int     `json:"trackedSessions"`
}

// DumpState implements [fwkplugin.StateDumper] for /debug/plugins/state.
func (p *Plugin) DumpState() (json.RawMessage, error) {
	logMean, logStd, observed := p.estimator.snapshot()
	return json.Marshal(debugState{
		LogMean:          logMean,
		LogStd:           logStd,
		Observations:     observed,
		RetentionSeconds: p.retentionDuration().Seconds(),
		TrackedSessions:  p.tracker.size(),
	})
}

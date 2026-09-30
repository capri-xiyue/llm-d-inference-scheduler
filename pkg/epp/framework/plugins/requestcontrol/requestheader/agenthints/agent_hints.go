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

// Package agenthints provides a RequestHeaderProcessor plugin that resolves
// per-request agent serving hints (priority, strict priority, expected output
// length, speculative prefill) from request headers and from the
// `llmdext.agent_hints` body object, and publishes each hint as a typed request
// attribute.
//
// The plugin only produces signals. It does not change routing, admission,
// or the forwarded body unless `stripFromBody` is enabled. Downstream
// consumers (flow control ordering, the in-flight token estimator, a
// speculative-prefill response processor, ...) read the attributes via
// scheduling.ReadRequestAttribute.
//
// The hint names and semantics mirror NVIDIA Dynamo's agent hints; the body
// envelope is llm-d's own:
//
//	{"llmdext": {"agent_hints": {"priority": 5, "strict_priority": 1,
//	                             "osl": 1024, "speculative_prefill": true}}}
//
// Header precedence follows Dynamo as well: per field, a well-formed header
// wins over the body; a missing or malformed header falls back to the body.
package agenthints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	"github.com/llm-d/llm-d-router/pkg/epp/metadata"
)

const (
	// PluginType is the plugin type name used in the EPP config.
	PluginType = "agent-hints"

	// BodyExtensionKey is the top-level request-body object that carries
	// llm-d request extensions.
	BodyExtensionKey = "llmdext"
	// BodyHintsKey is the sub-object of BodyExtensionKey that carries agent hints.
	BodyHintsKey = "agent_hints"

	bodyPriorityField           = "priority"
	bodyStrictPriorityField     = "strict_priority"
	bodyOSLField                = "osl"
	bodySpeculativePrefillField = "speculative_prefill"
)

// Request-attribute keys published by this plugin. Each hint is published
// under its own key so consumers can declare a dependency on exactly the
// hint they need. The keys carry no producer name: configuration names them
// bare ("attribute: agent-hint-priority").
var (
	// PriorityKey holds the unified request priority as an int. Higher values
	// mean more urgent, matching the InferenceObjective convention and
	// Dynamo's unified semantics. Negative values are preserved; consumers
	// decide how to clamp.
	PriorityKey = plugin.NewDataKey("agent-hint-priority", "")
	// StrictPriorityKey holds the strict queue tier as an int (always >= 0).
	StrictPriorityKey = plugin.NewDataKey("agent-hint-strict-priority", "")
	// OSLKey holds the expected output sequence length in tokens as an int
	// (always > 0).
	OSLKey = plugin.NewDataKey("agent-hint-osl", "")
	// SpeculativePrefillKey holds the speculative-prefill request as a bool.
	SpeculativePrefillKey = plugin.NewDataKey("agent-hint-speculative-prefill", "")
)

// Parameters is the user-facing plugin configuration block.
type Parameters struct {
	// StripFromBody removes `llmdext.agent_hints` from the forwarded request
	// body after the hints have been read, so backends that reject unknown
	// fields never see it. `llmdext` itself is removed when it becomes empty.
	// Default: false (body forwarded unchanged).
	StripFromBody bool `json:"stripFromBody,omitempty"`
}

// PluginFactory is the factory function for the agent hints plugin.
func PluginFactory(name string, rawParameters *json.Decoder, _ plugin.Handle) (plugin.Plugin, error) {
	var params Parameters
	if rawParameters != nil {
		if err := rawParameters.Decode(&params); err != nil {
			return nil, fmt.Errorf("%s: failed to decode parameters: %w", PluginType, err)
		}
	}
	return &Plugin{
		typedName:     plugin.TypedName{Type: PluginType, Name: name},
		stripFromBody: params.StripFromBody,
	}, nil
}

// compile-time interface assertions
var (
	_ requestcontrol.RequestHeaderProcessor = &Plugin{}
	_ plugin.ProducerPlugin                 = &Plugin{}
)

// Plugin resolves agent hints from headers and body and publishes them as
// request attributes.
type Plugin struct {
	typedName     plugin.TypedName
	stripFromBody bool
}

func (p *Plugin) TypedName() plugin.TypedName {
	return p.typedName
}

// Produces declares the typed request attributes written by this plugin.
// The zero values fix the Go types consumers must declare in Consumes().
func (p *Plugin) Produces() map[plugin.DataKey]any {
	return map[plugin.DataKey]any{
		PriorityKey:           int(0),
		StrictPriorityKey:     int(0),
		OSLKey:                int(0),
		SpeculativePrefillKey: false,
	}
}

// Hints is the resolved set of agent hints for one request. A nil field
// means the hint was not supplied (or was supplied but invalid) on either
// the headers or the body.
type Hints struct {
	Priority           *int
	StrictPriority     *int
	OSL                *int
	SpeculativePrefill *bool
}

// RequestHeader runs after the request body is parsed and attached, but
// before admission control. It never fails the request: malformed values are
// logged at debug level and skipped.
func (p *Plugin) RequestHeader(ctx context.Context, request *scheduling.InferenceRequest) error {
	if request == nil {
		return nil
	}
	logger := log.FromContext(ctx).V(logutil.DEBUG)

	payload, bodyHints := bodyHintsOf(request.Body)
	hints := Resolve(request.Headers, bodyHints, func(source, field, raw string, err error) {
		logger.Info("agent-hints: ignoring invalid hint value",
			"source", source, "field", field, "value", raw, "err", err)
	})

	if hints.Priority != nil {
		request.PutAttribute(PriorityKey, *hints.Priority)
	}
	if hints.StrictPriority != nil {
		request.PutAttribute(StrictPriorityKey, *hints.StrictPriority)
	}
	if hints.OSL != nil {
		request.PutAttribute(OSLKey, *hints.OSL)
	}
	if hints.SpeculativePrefill != nil {
		request.PutAttribute(SpeculativePrefillKey, *hints.SpeculativePrefill)
	}

	if p.stripFromBody && bodyHints != nil {
		stripBodyHints(payload)
		request.Body.Mutated = true
	}
	return nil
}

// invalidFn is called by Resolve for every value that was present but could
// not be parsed. source is "header" or "body".
type invalidFn func(source, field, raw string, err error)

// Resolve merges header and body hints with per-field header-over-body
// precedence. headers must use lower-case keys (as stored by the EPP request
// handler). body is the `llmdext.agent_hints` object, or nil.
func Resolve(headers map[string]string, body map[string]any, onInvalid invalidFn) Hints {
	if onInvalid == nil {
		onInvalid = func(string, string, string, error) {}
	}
	var h Hints

	h.Priority = resolveInt(headers, metadata.RequestPriorityHeaderKey, body, bodyPriorityField,
		math.MinInt32, math.MaxInt32, onInvalid)
	h.StrictPriority = resolveInt(headers, metadata.RequestStrictPriorityHeaderKey, body, bodyStrictPriorityField,
		0, math.MaxUint32, onInvalid)
	h.OSL = resolveInt(headers, metadata.ExpectedOutputTokensHeaderKey, body, bodyOSLField,
		1, math.MaxUint32, onInvalid)
	h.SpeculativePrefill = resolveBool(headers, metadata.SpeculativePrefillHeaderKey, body, bodySpeculativePrefillField,
		onInvalid)
	return h
}

// resolveInt returns the header value when present and valid, else the body
// value when present and valid, else nil. Range is inclusive.
func resolveInt(headers map[string]string, headerKey string, body map[string]any, bodyKey string,
	minVal, maxVal int64, onInvalid invalidFn) *int {
	if raw, ok := metadata.GetLowerCaseHeaderValue(headers, headerKey); ok {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil {
			if v < minVal || v > maxVal {
				err = fmt.Errorf("out of range [%d, %d]", minVal, maxVal)
			}
		}
		if err == nil {
			out := int(v)
			return &out
		}
		onInvalid("header", headerKey, raw, err)
	}
	if body == nil {
		return nil
	}
	rawAny, ok := body[bodyKey]
	if !ok {
		return nil
	}
	v, err := int64FromAny(rawAny)
	if err == nil && (v < minVal || v > maxVal) {
		err = fmt.Errorf("out of range [%d, %d]", minVal, maxVal)
	}
	if err != nil {
		onInvalid("body", bodyKey, fmt.Sprint(rawAny), err)
		return nil
	}
	out := int(v)
	return &out
}

// resolveBool mirrors resolveInt for boolean hints. Headers accept anything
// strconv.ParseBool accepts ("true", "1", "false", "0", ...). Body values
// must be JSON booleans.
func resolveBool(headers map[string]string, headerKey string, body map[string]any, bodyKey string,
	onInvalid invalidFn) *bool {
	if raw, ok := metadata.GetLowerCaseHeaderValue(headers, headerKey); ok {
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err == nil {
			return &v
		}
		onInvalid("header", headerKey, raw, err)
	}
	if body == nil {
		return nil
	}
	rawAny, ok := body[bodyKey]
	if !ok {
		return nil
	}
	v, isBool := rawAny.(bool)
	if !isBool {
		onInvalid("body", bodyKey, fmt.Sprint(rawAny), errors.New("expected JSON boolean"))
		return nil
	}
	return &v
}

// int64FromAny converts a JSON-decoded number to int64. encoding/json decodes
// numbers into float64 by default, so that is the primary case; json.Number
// and Go integer types are accepted for callers that decode differently.
func int64FromAny(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("expected integer, got %v", n)
		}
		if n < math.MinInt64 || n > math.MaxInt64 {
			return 0, fmt.Errorf("integer overflow: %v", n)
		}
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint32:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("expected JSON number, got %T", v)
	}
}

// bodyHintsOf returns the parsed payload map and its `llmdext.agent_hints`
// object. Either may be nil when the body was not parsed into a map or the
// object is absent or not an object.
func bodyHintsOf(body *fwkrh.InferenceRequestBody) (fwkrh.PayloadMap, map[string]any) {
	if body == nil || body.Payload == nil {
		return nil, nil
	}
	payload, ok := body.Payload.AsMap()
	if !ok || payload == nil {
		return nil, nil
	}
	ext, ok := payload[BodyExtensionKey].(map[string]any)
	if !ok {
		return payload, nil
	}
	hints, ok := ext[BodyHintsKey].(map[string]any)
	if !ok {
		return payload, nil
	}
	return payload, hints
}

// stripBodyHints deletes `llmdext.agent_hints` from payload, and `llmdext` too
// when nothing else remains under it.
func stripBodyHints(payload fwkrh.PayloadMap) {
	ext, ok := payload[BodyExtensionKey].(map[string]any)
	if !ok {
		return
	}
	delete(ext, BodyHintsKey)
	if len(ext) == 0 {
		delete(payload, BodyExtensionKey)
	}
}

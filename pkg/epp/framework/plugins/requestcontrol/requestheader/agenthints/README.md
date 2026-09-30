# Agent Hints

**Type:** `agent-hints`
**Interfaces:** `requestcontrol.RequestHeaderProcessor`, `plugin.ProducerPlugin`
**Stability:** Alpha

Resolves per-request *agent serving hints* from request headers and from the
`llmdext.agent_hints` body object, and publishes each hint as a typed
request attribute. The plugin only produces signals: it does not change
admission, scheduling, or (by default) the forwarded body. It is the hint
counterpart of [`agent-identity`](../agentidentity/README.md): `agent-identity`
says *who* is asking, `agent-hints` says *what the request wants*.

The hint names and semantics mirror [NVIDIA Dynamo's agent hints](https://github.com/ai-dynamo/dynamo).
The envelopes are llm-d's own: headers use the `x-llm-d-*` prefix and the body
object is `llmdext`. Dynamo's `x-dynamo-*` headers and `llmdext` body object are
not read.

## Hints

| Hint | Header | Body field | Attribute | Type | Validation |
|------|--------|------------|-----------|------|------------|
| Priority | `x-llm-d-request-priority` | `llmdext.agent_hints.priority` | `agent-hint-priority` | `int` | int32 range; negatives preserved |
| Strict priority | `x-llm-d-request-strict-priority` | `llmdext.agent_hints.strict_priority` | `agent-hint-strict-priority` | `int` | `>= 0` |
| Expected output length | `x-llm-d-expected-output-tokens` | `llmdext.agent_hints.osl` | `agent-hint-osl` | `int` | `> 0` |
| Speculative prefill | `x-llm-d-speculative-prefill` | `llmdext.agent_hints.speculative_prefill` | `agent-hint-speculative-prefill` | `bool` | JSON boolean in body; `strconv.ParseBool` forms in header |

Semantics match Dynamo's unified convention: for `priority`, **higher means more
urgent** (the same polarity as `InferenceObjective.spec.priority` and SGLang;
the EPP's existing `RewritePriority` already negates for vLLM targets when
priority propagation is enabled). `strict_priority` is a queue tier: higher
tiers are meant to always precede lower tiers. `osl` is the client's estimate
of output tokens. `speculative_prefill` asks the serving stack to warm the
next-turn prefix once this turn completes.

Dynamo's deprecated `latency_sensitivity` field is intentionally not read.

## Precedence

Per field, a well-formed header wins over the body. A missing or malformed
header falls back to the body value. A malformed value on both sides leaves the
attribute unset. Malformed values are logged at debug level and never fail the
request.

| Header | Body | Result |
|--------|------|--------|
| `9` | `2` | `9` |
| absent | `2` | `2` |
| `high` (invalid) | `2` | `2` |
| `high` (invalid) | absent | attribute not set |
| `9` | absent | `9` |

## What It Does

1. Runs after the request body has been parsed, before admission control, like
   every `RequestHeaderProcessor`.
2. Reads `request.Headers` (lower-cased keys) and, when the body was parsed into
   a JSON map (`request.Body.Payload.AsMap()`), the `llmdext.agent_hints` object.
   Raw or proto payloads yield no body hints.
3. Publishes one attribute per successfully resolved hint via
   `request.PutAttribute`. Hints that are absent or invalid are not published,
   so `ReadRequestAttribute` returning `ok == false` means "no hint".
4. Optionally (`stripFromBody: true`) deletes `llmdext.agent_hints` from the
   forwarded body and marks the body mutated so it is re-serialized. `llmdext`
   itself is dropped when it becomes empty; sibling `llmdext` fields are kept.

The plugin never sets `FairnessID`, `Objectives.Priority`, or any scheduling
state. Wiring the hints into flow control, scorers, or response processing is
the job of dedicated consumers.

## Inputs Consumed

- `scheduling.InferenceRequest.Headers` — the four headers above.
- `scheduling.InferenceRequest.Body.Payload` — read-only unless `stripFromBody`
  is set.

## Outputs Produced

Request attributes, declared in `Produces()` with the Go types consumers must
use in `Consumes()`:

```go
agenthints.PriorityKey            // int
agenthints.StrictPriorityKey      // int
agenthints.OSLKey                 // int
agenthints.SpeculativePrefillKey  // bool
```

Consumers read them with, for example:

```go
if osl, ok := scheduling.ReadRequestAttribute[int](req, agenthints.OSLKey); ok { ... }
```

## Configuration

**Location:** top-level `plugins:` list in the `EndpointPickerConfig`.
**Enabled by default:** No. Alpha plugins must be allowed by the EPP process.

### Parameters

| Name | Type | Required | Default | Description |
|------|------|----------|---------|-------------|
| `stripFromBody` | `bool` | No | `false` | Remove `llmdext.agent_hints` from the forwarded request body after reading it. Leave off for vLLM, which tolerates unknown fields; turn on for backends that reject them. |

### Examples

```yaml
apiVersion: llm-d.ai/v1
kind: EndpointPickerConfig
plugins:
  - type: agent-identity
  - type: agent-hints
```

```yaml
plugins:
  - type: agent-hints
    parameters:
      stripFromBody: true
```

### Sending hints

```bash
# Body
curl http://<gateway>/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "m",
  "messages": [{"role": "user", "content": "..."}],
  "llmdext": {"agent_hints": {"priority": 5, "osl": 1024, "speculative_prefill": true}}
}'

# Headers (override body per field)
curl http://<gateway>/v1/chat/completions \
  -H 'x-llm-d-request-priority: 9' \
  -H 'x-llm-d-expected-output-tokens: 256' \
  -d '{"model": "m", "messages": [...]}'
```

Headers must survive any proxy in front of the gateway. LiteLLM, for instance,
strips `x-*` headers unless `forward_client_headers_to_llm_api: true` is set.

## Consumers

None yet. The attributes are published so that follow-up work can attach:

- `agent-hint-priority` → per-request override of the flow-control priority and
  of the propagated backend `priority` field.
- `agent-hint-strict-priority` → a strict-tier ordering policy.
- `agent-hint-osl` → the in-flight token estimator, as an exact value ahead of
  the `outlen-bucket` estimate.
- `agent-hint-speculative-prefill` → a response-body processor that warms the
  next-turn prefix on the serving pod.

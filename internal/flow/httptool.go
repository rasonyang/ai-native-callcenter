// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A declarative tool is a template plus a way to read the answer. It exists so
// that connecting a flow to a backend is configuration rather than code — and
// so that the set of things a flow can do to a backend stays small and
// inspectable.

const (
	// httpToolTimeout bounds a backend call. This happens inside a phone call
	// with someone waiting, so a backend that has not answered in five seconds
	// has effectively failed.
	httpToolTimeout = 5 * time.Second
	// maxResponseBytes bounds what is read back, so a misbehaving backend
	// cannot exhaust memory across concurrent calls.
	maxResponseBytes = 1 << 20
)

// Backend runs a flow's declarative HTTP tools.
type Backend struct {
	baseURL string
	client  *http.Client
}

// NewBackend targets a base URL. An empty base URL leaves declarative tools
// unusable, which is reported per call rather than at startup so a flow with
// no HTTP tools still runs.
func NewBackend(baseURL string) *Backend {
	return &Backend{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  &http.Client{Timeout: httpToolTimeout},
	}
}

// Call runs one declarative tool and maps its response into a result.
func (b *Backend) Call(ctx context.Context, tool Tool, args, slots map[string]any) (Result, error) {
	if b.baseURL == "" {
		return Result{}, fmt.Errorf("no backend address is configured")
	}
	spec := tool.HTTP

	body, err := json.Marshal(renderTemplate(spec.Body, args, slots))
	if err != nil {
		return Result{}, fmt.Errorf("encode request: %w", err)
	}
	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}

	ctx, cancel := context.WithTimeout(ctx, httpToolTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, method, b.baseURL+spec.Path, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := b.client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("call %s: %w", spec.Path, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("read response from %s: %w", spec.Path, err)
	}
	if response.StatusCode >= 400 {
		return Result{}, fmt.Errorf("%s returned %d", spec.Path, response.StatusCode)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Result{}, fmt.Errorf("%s did not return an object: %w", spec.Path, err)
	}
	return resultFrom(spec, decoded), nil
}

// resultFrom applies the success test and the field mapping.
func resultFrom(spec *HTTPCall, response map[string]any) Result {
	isOK := true
	if spec.SuccessWhen != nil {
		actual, _ := lookupPath(response, spec.SuccessWhen.Path)
		isOK = valuesEqual(actual, spec.SuccessWhen.Equals)
	}

	if !isOK {
		message := "the backend rejected the request"
		if spec.ErrorFrom != "" {
			if reported, ok := lookupPath(response, spec.ErrorFrom); ok {
				message = asString(reported)
			}
		}
		return Result{IsOK: false, Error: message}
	}

	// Only the mapped fields are exposed. Handing the model a whole backend
	// response would put whatever that response happens to contain — including
	// data about other people — into a conversation.
	fields := make(map[string]any, len(spec.Result))
	for name, path := range spec.Result {
		if value, ok := lookupPath(response, path); ok {
			fields[name] = value
		}
	}
	return Result{IsOK: true, Fields: fields}
}

// lookupPath walks a dotted path through a decoded JSON object.
func lookupPath(document map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	var current any = document
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

var templateReference = regexp.MustCompile(`\{(args|slots)\.([A-Za-z0-9_.\-]+)\}`)

// renderTemplate substitutes {args.x} and {slots.y} through a request body.
//
// A value that is nothing but a single reference keeps the referenced value's
// type, so a number stays a number rather than becoming the string "1"; a
// reference embedded in surrounding text renders as text.
func renderTemplate(template map[string]any, args, slots map[string]any) map[string]any {
	if template == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(template))
	for key, value := range template {
		out[key] = renderValue(value, args, slots)
	}
	return out
}

func renderValue(value any, args, slots map[string]any) any {
	switch v := value.(type) {
	case string:
		return renderString(v, args, slots)
	case map[string]any:
		return renderTemplate(v, args, slots)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = renderValue(item, args, slots)
		}
		return out
	default:
		return value
	}
}

func renderString(template string, args, slots map[string]any) any {
	matches := templateReference.FindAllStringSubmatchIndex(template, -1)
	if len(matches) == 0 {
		return template
	}

	// A lone reference passes the value through with its type intact.
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(template) {
		return resolveReference(template[matches[0][2]:matches[0][3]],
			template[matches[0][4]:matches[0][5]], args, slots)
	}

	return templateReference.ReplaceAllStringFunc(template, func(match string) string {
		parts := templateReference.FindStringSubmatch(match)
		return asString(resolveReference(parts[1], parts[2], args, slots))
	})
}

func resolveReference(source, name string, args, slots map[string]any) any {
	if source == "args" {
		return args[name]
	}
	return slots[name]
}

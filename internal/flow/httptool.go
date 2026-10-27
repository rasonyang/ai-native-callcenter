// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/rasonyang/ai-native-callcenter/internal/egress"
)

// A declarative tool is a template plus a way to read the answer. It exists so
// that connecting a flow to a backend is configuration rather than code — and
// so that the set of things a flow can do to a backend stays small and
// inspectable.

// maxResponseBytes bounds what is read back, so a misbehaving backend cannot
// exhaust memory across concurrent calls.
const maxResponseBytes = 1 << 20

// Backend runs a flow's declarative HTTP tools. One serves every call of a
// process: it holds the connection pool and the deployment's outbound host
// policy, and is safe for concurrent use.
type Backend struct {
	// base is the deployment's base URL (AICC_BOT_BACKEND_BASE); nil when it
	// is unset or unusable.
	base *url.URL
	// isBasePinned makes every tool use base, its own baseUrl ignored.
	isBasePinned bool
	client       *http.Client
	dialer       *egress.Dialer
	secrets      func(string) (string, bool)
	log          *slog.Logger
}

// BackendOption configures a Backend.
type BackendOption func(*Backend)

// WithAllowedHosts sets the public hosts tools may reach
// (AICC_TOOL_ALLOWED_HOSTS). The base URL's host is allowed in any case.
func WithAllowedHosts(entries []egress.Entry) BackendOption {
	return func(b *Backend) {
		b.dialer.Policy = egress.NewPolicy(entries, baseHost(b.base))
	}
}

// WithSecrets sets where {secrets.NAME} resolves from; the default is the
// process environment.
func WithSecrets(lookup func(string) (string, bool)) BackendOption {
	return func(b *Backend) { b.secrets = lookup }
}

// WithLogger sets the logger used when a call brings none of its own.
func WithLogger(log *slog.Logger) BackendOption {
	return func(b *Backend) { b.log = log }
}

// WithPinnedBase makes every tool call the Backend's own base URL, whatever
// its baseUrl says. The flow simulator uses it so a scenario stays inside its
// mock backend.
func WithPinnedBase() BackendOption {
	return func(b *Backend) { b.isBasePinned = true }
}

// NewBackend targets a base URL. An empty base URL leaves tools without a
// baseUrl of their own unusable, which is reported per call rather than at
// startup so a flow with no HTTP tools still runs.
//
// Every connection goes through the outbound host policy: with no
// WithAllowedHosts, private and loopback addresses and the base URL's own host
// are reachable, and no other public host.
func NewBackend(baseURL string, opts ...BackendOption) *Backend {
	b := &Backend{secrets: os.LookupEnv, log: slog.Default()}
	var baseErr error
	if baseURL != "" {
		b.base, baseErr = deploymentBase(baseURL)
	}
	b.dialer = &egress.Dialer{Policy: egress.NewPolicy(nil, baseHost(b.base))}
	for _, opt := range opts {
		opt(b)
	}
	if baseErr != nil {
		// The value is not logged: an operator's base may carry a password.
		b.log.Error("AICC_BOT_BACKEND_BASE is not usable; HTTP tools without a baseUrl of their own will fail",
			"problem", baseErr.Error())
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// No proxy: a proxy would be the address dialled, and the policy would
	// judge the proxy rather than the backend.
	transport.Proxy = nil
	transport.DialContext = b.dialer.DialContext
	transport.DialTLSContext = nil
	b.client = &http.Client{
		Transport: transport,
		// A redirect is not followed: the place it points to was never
		// checked against anything the flow author or the operator wrote, and
		// following one is the classic way round a host policy. A 3xx is a
		// tool failure.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return b
}

// deploymentBase reads AICC_BOT_BACKEND_BASE. It is the operator's, not a
// flow's, so unlike a tool's baseUrl it may carry a user and password; the
// rest is the same: an absolute http or https URL with a host.
func deploymentBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("it is not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("it must be an absolute http:// or https:// URL")
	case u.Hostname() == "":
		return nil, errors.New("it names no host")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("it must not have a query or a fragment")
	}
	return u, nil
}

func baseHost(base *url.URL) string {
	if base == nil {
		return ""
	}
	return base.Hostname()
}

// Call runs one declarative tool and maps its response into a result. log is
// the call's logger; nil uses the Backend's.
//
// A value the model supplied that cannot go where the template puts it (an
// empty path segment, "..", an object) is a refusal the model reads and can
// correct, returned as a result. Anything that stops the call from being made
// or answered is an error.
func (b *Backend) Call(ctx context.Context, log *slog.Logger, tool Tool, args, slots map[string]any) (Result, error) {
	if log == nil {
		log = b.log
	}
	spec := tool.HTTP
	base := b.base
	if spec.BaseURL != "" && !b.isBasePinned {
		parsed, err := url.Parse(spec.BaseURL)
		if err != nil || baseURLProblem(spec.BaseURL) != "" {
			return Result{}, fmt.Errorf("the tool's baseUrl is not usable")
		}
		base = parsed
	}
	if base == nil {
		return Result{}, fmt.Errorf("no backend address is configured")
	}

	parts, problems := spec.urlParts()
	if len(problems) > 0 {
		// Load refuses these; a Tool built some other way gets the same answer.
		return Result{}, fmt.Errorf("the tool's path and query cannot be combined: %s", problems[0])
	}
	path, err := renderPath(parts.path, args, slots)
	if err != nil {
		return Failed(err.Error(), ""), nil
	}
	query, err := renderQuery(parts.query, args, slots)
	if err != nil {
		return Failed(err.Error(), ""), nil
	}
	target, err := buildURL(base, path, parts, query)
	if err != nil {
		return Failed(err.Error(), ""), nil
	}

	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	switch {
	case spec.Body != nil:
		encoded, err := json.Marshal(renderTemplate(spec.Body, args, slots))
		if err != nil {
			return Result{}, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	case sendsEmptyObject(method):
		// What every tool without a body template has always sent, and what
		// a backend reading a JSON body may depend on.
		body = strings.NewReader("{}")
	}

	timeout := spec.Timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return Result{}, fmt.Errorf("build request for %s: %w", parts.path, stripURL(err))
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, header := range sortedKeys(spec.Headers) {
		value, err := resolveSecrets(header, spec.Headers[header], b.secrets)
		if err != nil {
			return Result{}, err
		}
		request.Header.Set(header, value)
	}

	// What is logged is what the flow wrote: the base, the path template and
	// the names of the query parameters sent. The rendered values (a phone
	// number, an order id) reach the backend's own access log and no other.
	fields := []any{"tool", tool.Name(), "method", method,
		"baseUrl", (&url.URL{Scheme: base.Scheme, Host: base.Host, Path: base.Path}).String(),
		"path", parts.path, "queryParams", slices.Sorted(maps.Keys(query)),
		"timeoutMs", timeout.Milliseconds()}

	started := time.Now()
	response, err := b.client.Do(request)
	if err != nil {
		err = stripURL(err)
		if refusal, ok := egress.IsRefusal(err); ok {
			log.Warn("http tool connection refused by the outbound host policy", append(fields,
				"host", refusal.Host, "address", refusal.Address, "rule", refusal.Rule)...)
			return Result{}, fmt.Errorf("call %s: %w", parts.path, refusal)
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			log.Warn("http tool timed out", append(fields, "elapsedMs", time.Since(started).Milliseconds())...)
			return Result{}, fmt.Errorf("%s did not answer within %d ms", parts.path, timeout.Milliseconds())
		}
		log.Warn("http tool call failed", append(fields, "error", err)...)
		return Result{}, fmt.Errorf("call %s: %w", parts.path, err)
	}
	defer response.Body.Close()
	log.Info("http tool called", append(fields,
		"status", response.StatusCode, "elapsedMs", time.Since(started).Milliseconds())...)

	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return Result{}, fmt.Errorf("%s answered with a redirect (%d), which is not followed", parts.path, response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{}, fmt.Errorf("%s did not answer within %d ms", parts.path, timeout.Milliseconds())
		}
		return Result{}, fmt.Errorf("read response from %s: %w", parts.path, stripURL(err))
	}
	if response.StatusCode >= 400 {
		// The backend's error code tells INDEX_UNAVAILABLE from
		// UPSTREAM_TIMEOUT in the log. The body itself, and the backend's
		// message, are never logged or returned: they are not ours to relay.
		code := backendErrorCode(spec.ErrorFrom, payload)
		log.Warn("http tool answered with an error status", append(fields,
			"status", response.StatusCode, "backendError", code)...)
		if code != "" {
			return Result{}, fmt.Errorf("%s returned %d (%s)", parts.path, response.StatusCode, code)
		}
		return Result{}, fmt.Errorf("%s returned %d", parts.path, response.StatusCode)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Result{}, fmt.Errorf("%s did not return an object: %w", parts.path, err)
	}
	return resultFrom(spec, decoded), nil
}

// sendsEmptyObject reports a method whose request without a body template
// still carries "{}": POST, PUT and PATCH, as before path templates. GET,
// DELETE, HEAD and the rest send no body.
func sendsEmptyObject(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// stripURL drops the URL a *url.Error carries. That URL is the rendered one,
// with the caller's values in its path and query, and an error is logged and
// handed to the model.
func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// maxBackendErrorLen bounds the backend's error code as logged and returned.
const maxBackendErrorLen = 128

// backendErrorCode reads the scalar at errorFrom in an error answer's JSON
// object body. It returns "" when errorFrom is unset, the body is not an
// object, or the path does not end at a scalar. Control characters are removed
// and the code is cut to maxBackendErrorLen characters.
func backendErrorCode(errorFrom string, payload []byte) string {
	if errorFrom == "" {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	value, ok := lookupPath(decoded, errorFrom)
	if !ok {
		return ""
	}
	switch value.(type) {
	case string, float64, bool:
	default:
		return ""
	}
	code := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, asString(value))
	if runes := []rune(code); len(runes) > maxBackendErrorLen {
		code = string(runes[:maxBackendErrorLen])
	}
	return code
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
	var modelOnly map[string]any
	for name, mapping := range spec.ModelOnly {
		value, ok := mapping.extract(response)
		if !ok {
			continue
		}
		if modelOnly == nil {
			modelOnly = make(map[string]any, len(spec.ModelOnly))
		}
		modelOnly[name] = value
	}
	return Result{IsOK: true, Fields: fields, ModelOnly: modelOnly}
}

// extract reads one modelOnly entry from a response. A path that does not
// resolve, or a projection whose path is not an array, yields nothing. Array
// elements that are not objects are skipped; a field of an element whose path
// does not resolve is left out of that element.
func (f ModelOnlyField) extract(response map[string]any) (any, bool) {
	value, ok := lookupPath(response, f.Path)
	if !ok {
		return nil, false
	}
	if !f.IsProjection {
		return value, true
	}
	elements, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(elements))
	for _, element := range elements {
		object, ok := element.(map[string]any)
		if !ok {
			continue
		}
		projected := make(map[string]any, len(f.Each))
		for name, path := range f.Each {
			if v, ok := lookupPath(object, path); ok {
				projected[name] = v
			}
		}
		out = append(out, projected)
	}
	return out, true
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

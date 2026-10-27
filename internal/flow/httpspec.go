// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// What an HTTP tool's block may say, checked at load, and how its path and
// query are rendered on a call.
//
// The path and the query take values the model supplies. A value is therefore
// never trusted to keep to its place: each one is escaped into the single path
// segment or the single query value it stands for, the URL is assembled from
// url.URL fields rather than pasted together as text, and the host comes only
// from the base URL. A value such as "@evil.com/x" can then neither change the
// host nor add a segment.

// A path written before path templates existed keeps the wire form it had,
// when the base and the path were joined as text and parsed as one URL: a
// literal "?query" is sent as it was written, a "#fragment" is dropped, as an
// HTTP client drops it, and a space or a non-ASCII character in a literal
// segment is escaped. Flows are re-loaded on every call, so a path that used
// to work and stopped loading would fail every call on its number.

// urlParts is an HTTP tool's path and query, split as they are sent.
type urlParts struct {
	// path is the path template with any literal query and fragment removed.
	path string
	// rawQuery is a literal query written in the path with no reference in
	// it. It is sent exactly as written, as it always was.
	rawQuery string
	// isQueryForced keeps a path that ends in a bare "?" sending it.
	isQueryForced bool
	// query is every templated query parameter: http.query, plus the
	// parameters of a literal query in the path that references a value.
	query map[string]string
}

// urlParts splits the path and merges the query. problems are what makes the
// two irreconcilable: a parameter given both in the path and in http.query,
// or a templated literal query that cannot be read as parameters.
func (h *HTTPCall) urlParts() (urlParts, []string) {
	parts := urlParts{path: h.Path, query: h.Query}
	var problems []string
	parts.path, _, _ = strings.Cut(parts.path, "#")
	path, literalQuery, hasQuery := strings.Cut(parts.path, "?")
	parts.path = path
	if !hasQuery || literalQuery == "" {
		parts.isQueryForced = hasQuery
		return parts, nil
	}

	merged := make(map[string]string, len(h.Query))
	maps.Copy(merged, h.Query)
	conflict := func(key string) {
		if _, ok := h.Query[key]; ok {
			problems = append(problems, fmt.Sprintf(
				"query parameter %q is given both in the path and in http.query", key))
		}
	}
	if !templateReference.MatchString(literalQuery) {
		parts.rawQuery = literalQuery
		for pair := range strings.FieldsFuncSeq(literalQuery, func(r rune) bool { return r == '&' || r == ';' }) {
			key, _, _ := strings.Cut(pair, "=")
			if unescaped, err := url.QueryUnescape(key); err == nil {
				key = unescaped
			}
			conflict(key)
		}
		return parts, problems
	}

	values, err := url.ParseQuery(literalQuery)
	if err != nil {
		return parts, []string{"the query written in the path references a value but cannot be read as parameters; move it to http.query"}
	}
	for _, key := range sortedKeys(values) {
		if len(values[key]) > 1 {
			problems = append(problems, fmt.Sprintf(
				"query parameter %q is repeated in the path; move it to http.query", key))
		}
		conflict(key)
		merged[key] = values[key][0]
	}
	parts.query = merged
	return parts, problems
}

// escapeLiteralPath is the wire form of a literal stretch of a path, as the
// URL parser has always produced it: a valid escape is kept, a space or a
// non-ASCII character is escaped. It fails only on what the parser refuses,
// such as a malformed % escape, which never reached a backend.
func escapeLiteralPath(literal string) (string, error) {
	if literal == "" {
		return "", nil
	}
	u, err := url.Parse("http://h/" + literal)
	if err != nil {
		return "", err
	}
	return u.EscapedPath()[1:], nil
}

// headerName is an HTTP field name (RFC 9110 token).
var headerName = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// reservedHeaders are set by the HTTP client from the request itself; a flow
// that set them would either be ignored or break the framing.
var reservedHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "te", "upgrade", "trailer", "keep-alive"}

// validateHTTPCall checks one HTTP tool's block beyond the presence of a path.
func validateHTTPCall(d declaredSlots, name string, tool Tool, report func(string, ...any)) {
	call := tool.HTTP
	declared := toolArgNames(tool)
	required := toolRequiredArgs(tool)

	if call.BaseURL != "" {
		if problem := baseURLProblem(call.BaseURL); problem != "" {
			report("tool %q baseUrl %s", name, problem)
		}
	}
	if t := call.TimeoutMs; t != nil && (*t < MinToolTimeoutMs || *t > MaxToolTimeoutMs) {
		report("tool %q timeoutMs is %d; it is a number of milliseconds from %d to %d",
			name, *t, MinToolTimeoutMs, MaxToolTimeoutMs)
	}

	// The path.
	parts, problems := call.urlParts()
	for _, problem := range problems {
		report("tool %q %s", name, problem)
	}
	if call.Path != "" {
		if !strings.HasPrefix(call.Path, "/") {
			report("tool %q path must start with a literal \"/\"", name)
		}
		if mentionsSecret(call.Path) {
			report("tool %q path references a secret; {secrets.NAME} may appear only in a header value", name)
		}
		for _, literal := range templateReference.Split(parts.path, -1) {
			if _, err := escapeLiteralPath(literal); err != nil {
				report("tool %q path is not a valid URL path (a malformed %% escape?)", name)
				break
			}
		}
		for _, m := range templateReference.FindAllStringSubmatch(parts.path, -1) {
			switch {
			case m[1] == "args" && !slices.Contains(declared, m[2]):
				report("tool %q path references {args.%s}, which is not a declared parameter of the tool", name, m[2])
			case m[1] == "args" && !slices.Contains(required, m[2]):
				report("tool %q path references {args.%s}, which the tool's parameters do not require; a path segment cannot be left out, so list it in required or move it to http.query", name, m[2])
			case m[1] == "slots" && !d.has(m[2]):
				report("tool %q path references {slots.%s}, %s", name, m[2], d.unwrittenReason(m[2]))
			}
		}
	}

	// The query: http.query and any templated query written in the path.
	for _, param := range sortedKeys(parts.query) {
		value := parts.query[param]
		if strings.TrimSpace(param) == "" {
			report("tool %q has a query parameter with no name", name)
		}
		if mentionsSecret(value) {
			report("tool %q query parameter %q references a secret; {secrets.NAME} may appear only in a header value", name, param)
		}
		for _, m := range templateReference.FindAllStringSubmatch(value, -1) {
			switch {
			case m[1] == "args" && !slices.Contains(declared, m[2]):
				report("tool %q query parameter %q references {args.%s}, which is not a declared parameter of the tool", name, param, m[2])
			case m[1] == "slots" && !d.has(m[2]):
				report("tool %q query parameter %q references {slots.%s}, %s", name, param, m[2], d.unwrittenReason(m[2]))
			}
		}
	}

	// The headers.
	seen := map[string]string{}
	for _, header := range sortedKeys(call.Headers) {
		value := call.Headers[header]
		lower := strings.ToLower(header)
		switch {
		case !headerName.MatchString(header):
			report("tool %q header %q is not a valid header name", name, header)
		case slices.Contains(reservedHeaders, lower):
			report("tool %q header %s is set by the HTTP client and cannot be given", name, header)
		}
		if other, dup := seen[lower]; dup {
			report("tool %q declares header %s twice (as %s and %s)", name, header, other, header)
		}
		seen[lower] = header
		if strings.ContainsAny(value, "\r\n\x00") {
			report("tool %q header %s has a line break or a NUL in its value", name, header)
		}
		if templateReference.MatchString(value) {
			report("tool %q header %s references {args.x} or {slots.y}; a header takes a literal or {secrets.NAME} only", name, header)
		}
		if hasMalformedSecret(value) {
			report("tool %q header %s has a malformed secret reference; write {secrets.NAME} with NAME in capital letters, digits and underscores", name, header)
		}
		if isCredentialHeader(header) && len(secretNames(value)) == 0 {
			report("tool %q header %s carries a credential as a literal; a flow is readable by everyone with config:read, so reference it as {secrets.NAME} and set %sNAME on the deployment", name, header, SecretEnvPrefix)
		}
	}

	validateModelOnly(name, call, report)

	if bodyMentionsSecret(call.Body) {
		report("tool %q body references a secret; {secrets.NAME} may appear only in a header value", name)
	}
	if tool.Collect != nil && mentionsSecret(tool.Collect.Value) {
		report("tool %q collect value references a secret; {secrets.NAME} may appear only in a header value", name)
	}
}

// validateModelOnly checks http.modelOnly. Its names share the output object
// with ok, error, hint and the result fields, so none may collide.
func validateModelOnly(name string, call *HTTPCall, report func(string, ...any)) {
	for _, field := range sortedKeys(call.ModelOnly) {
		mapping := call.ModelOnly[field]
		switch {
		case strings.TrimSpace(field) == "":
			report("tool %q modelOnly has an output with no name", name)
		case field == "ok" || field == "error" || field == "hint":
			report("tool %q modelOnly output %q is a field of every tool output; choose another name", name, field)
		}
		if _, clash := call.Result[field]; clash {
			report("tool %q modelOnly output %q is also a key of result; both end up in the same tool output, so give each its own name", name, field)
		}
		if strings.TrimSpace(mapping.Path) == "" {
			report("tool %q modelOnly output %q has an empty path", name, field)
		}
		if !mapping.IsProjection {
			continue
		}
		if len(mapping.Each) == 0 {
			report("tool %q modelOnly output %q lists no fields in each", name, field)
		}
		for _, inner := range sortedKeys(mapping.Each) {
			if strings.TrimSpace(inner) == "" {
				report("tool %q modelOnly output %q has an each field with no name", name, field)
			}
			if strings.TrimSpace(mapping.Each[inner]) == "" {
				report("tool %q modelOnly output %q each field %q has an empty path", name, field, inner)
			}
		}
	}
}

// baseURLProblem says what is wrong with a tool's baseUrl, or "" when nothing.
func baseURLProblem(raw string) string {
	if mentionsSecret(raw) || templateReference.MatchString(raw) {
		return "is a template; it must be a literal URL"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "is not a URL"
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Sprintf("%q must be an absolute http:// or https:// URL", raw)
	case u.Host == "" || u.Hostname() == "":
		return fmt.Sprintf("%q names no host", raw)
	case u.User != nil:
		return "carries a user name or password; put a credential in a header as {secrets.NAME}"
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Sprintf("%q has a query; put query parameters in http.query", raw)
	case u.Fragment != "":
		return fmt.Sprintf("%q has a fragment", raw)
	}
	return ""
}

func bodyMentionsSecret(v any) bool {
	switch x := v.(type) {
	case string:
		return mentionsSecret(x)
	case map[string]any:
		for key, item := range x {
			if mentionsSecret(key) || bodyMentionsSecret(item) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, bodyMentionsSecret)
	}
	return false
}

// toolRequiredArgs lists the arguments a tool's schema requires.
func toolRequiredArgs(tool Tool) []string {
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.ModelParameters(), &schema); err != nil {
		return nil
	}
	return schema.Required
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// renderPath substitutes the path's references, each escaped into one path
// segment, and returns the escaped path. A value that would not be a segment
// at all (empty, "." or "..") or is not a single value (an object, a list) is
// refused before anything is sent; the refusal names the reference, never the
// value.
func renderPath(template string, args, slots map[string]any) (string, error) {
	var out strings.Builder
	last := 0
	for _, m := range templateReference.FindAllStringSubmatchIndex(template, -1) {
		literal, err := escapeLiteralPath(template[last:m[0]])
		if err != nil {
			return "", fmt.Errorf("the path is not a valid URL path")
		}
		out.WriteString(literal)
		source, name := template[m[2]:m[3]], template[m[4]:m[5]]
		text, problem := pathValue(resolveReference(source, name, args, slots))
		if problem != "" {
			return "", fmt.Errorf("the value for {%s.%s} in the path %s", source, name, problem)
		}
		out.WriteString(url.PathEscape(text))
		last = m[1]
	}
	literal, err := escapeLiteralPath(template[last:])
	if err != nil {
		return "", fmt.Errorf("the path is not a valid URL path")
	}
	out.WriteString(literal)
	return out.String(), nil
}

func pathValue(value any) (string, string) {
	switch value.(type) {
	case map[string]any, []any:
		return "", "is an object or a list, not a single value"
	}
	text := asString(value)
	switch text {
	case "":
		return "", "is empty"
	case ".", "..":
		return "", "is \".\" or \"..\", which is not a path segment"
	}
	return text, ""
}

// renderQuery renders the query parameters. A parameter whose template
// references an argument the model did not pass is left out; a value that is
// an object or a list is refused.
func renderQuery(query map[string]string, args, slots map[string]any) (url.Values, error) {
	values := url.Values{}
	for _, param := range sortedKeys(query) {
		template := query[param]
		isAbsent := false
		for _, m := range templateReference.FindAllStringSubmatch(template, -1) {
			value := resolveReference(m[1], m[2], args, slots)
			if m[1] == "args" && value == nil {
				isAbsent = true
			}
			switch value.(type) {
			case map[string]any, []any:
				return nil, fmt.Errorf("the value for {%s.%s} in query parameter %s is an object or a list, not a single value", m[1], m[2], param)
			}
		}
		if isAbsent {
			continue
		}
		values.Set(param, asString(renderString(template, args, slots)))
	}
	return values, nil
}

// buildURL puts the base, the rendered path and the query together as URL
// fields. The base's scheme, user and host are taken as they are (only the
// deployment's own base may carry a user); the path is appended to the base's
// own path.
func buildURL(base *url.URL, escapedPath string, parts urlParts, query url.Values) (*url.URL, error) {
	u := *base
	u.Fragment, u.RawFragment = "", ""
	raw := strings.TrimSuffix(base.EscapedPath(), "/") + escapedPath
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return nil, fmt.Errorf("the path is not a valid escaped URL path")
	}
	u.Path, u.RawPath = decoded, raw
	u.RawQuery = parts.rawQuery
	if encoded := query.Encode(); encoded != "" {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += encoded
	}
	u.ForceQuery = parts.isQueryForced
	return &u, nil
}

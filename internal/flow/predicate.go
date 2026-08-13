// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"fmt"
	"strconv"
	"strings"
)

// Conditions are structured rather than expressions on purpose: a flow file is
// configuration, and an expression language in configuration is an execution
// engine nobody validated. These operators are a closed set, checked at load
// time, so a typo is a startup error rather than a call that behaves oddly.

// evaluate reports whether a condition holds. A nil condition always holds,
// which is how an unconditional fallback rule is written.
func evaluate(condition *Condition, slots map[string]any) (bool, error) {
	if condition == nil {
		return true, nil
	}

	if len(condition.All) > 0 {
		for _, sub := range condition.All {
			ok, err := evaluate(&sub, slots)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
	if len(condition.Any) > 0 {
		for _, sub := range condition.Any {
			ok, err := evaluate(&sub, slots)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	return evaluateLeaf(*condition, slots)
}

func evaluateLeaf(condition Condition, slots map[string]any) (bool, error) {
	if !knownOperators[condition.Op] {
		return false, fmt.Errorf("unknown operator %q", condition.Op)
	}
	if condition.Slot == "" {
		return false, fmt.Errorf("condition with operator %s names no slot", condition.Op)
	}
	actual, isPresent := slots[condition.Slot]

	switch condition.Op {
	case OpEqual:
		return valuesEqual(actual, condition.Value), nil
	case OpNotEqual:
		return !valuesEqual(actual, condition.Value), nil
	case OpGreaterThan, OpLessThan, OpGreaterThanOrEqual, OpLessThanOrEqual:
		return compare(condition.Op, actual, condition.Value), nil
	case OpIsNull:
		return !isPresent || actual == nil, nil
	case OpIsNotNull:
		return isPresent && actual != nil, nil
	case OpIsEmpty:
		return isEmpty(actual), nil
	case OpIsNotEmpty:
		return !isEmpty(actual), nil
	case OpIn:
		return membership(condition.Value, actual), nil
	case OpNotIn:
		return !membership(condition.Value, actual), nil
	case OpContains:
		return actual != nil && strings.Contains(asString(actual), asString(condition.Value)), nil
	case OpNotContains:
		return actual == nil || !strings.Contains(asString(actual), asString(condition.Value)), nil
	default:
		return false, fmt.Errorf("unhandled operator %q", condition.Op)
	}
}

// valuesEqual compares with numeric tolerance, because backends routinely
// return "1" where a flow author wrote 1, and a flow that behaves differently
// depending on which one they typed would be a trap.
func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a == b {
		return true
	}
	if na, okA := asNumber(a); okA {
		if nb, okB := asNumber(b); okB {
			return na == nb
		}
	}
	return asString(a) == asString(b)
}

// compare orders numerically when both sides are numbers and lexically
// otherwise. A missing slot never satisfies an ordering test.
func compare(op Operator, actual, target any) bool {
	if actual == nil {
		return false
	}

	var result int
	if na, okA := asNumber(actual); okA {
		if nb, okB := asNumber(target); okB {
			switch {
			case na < nb:
				result = -1
			case na > nb:
				result = 1
			}
		} else {
			result = strings.Compare(asString(actual), asString(target))
		}
	} else {
		result = strings.Compare(asString(actual), asString(target))
	}

	switch op {
	case OpGreaterThan:
		return result > 0
	case OpLessThan:
		return result < 0
	case OpGreaterThanOrEqual:
		return result >= 0
	case OpLessThanOrEqual:
		return result <= 0
	default:
		return false
	}
}

// isEmpty treats absence, the empty string, and empty collections as empty.
// A false boolean and a zero number are values, not emptiness.
func isEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// membership tests a list target, or substring containment when the target is
// a plain string.
func membership(target, actual any) bool {
	if list, ok := target.([]any); ok {
		for _, item := range list {
			if valuesEqual(actual, item) {
				return true
			}
		}
		return false
	}
	return actual != nil && strings.Contains(asString(target), asString(actual))
}

func asString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprint(v)
	}
}

func asNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

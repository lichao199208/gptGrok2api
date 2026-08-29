package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// solveSentinelTurnstileToken mirrors the small operation VM used by
// ChatGPT's sentinel endpoint. The dx value is an XOR-obfuscated operation
// queue; it is not a browser cookie and must be computed for the same p value.
func solveSentinelTurnstileToken(dx, p string) (string, error) {
	raw, err := decodeSentinelDX(dx, p)
	if err != nil {
		return "", err
	}
	var tokenList []any
	if err := json.Unmarshal(raw, &tokenList); err != nil {
		return "", fmt.Errorf("decode sentinel operation queue: %w", err)
	}

	vm := &sentinelVM{
		values:    map[any]any{},
		startTime: time.Now(),
		result:    "",
	}
	vm.values[float64(9)] = tokenList
	vm.values[float64(10)] = "window"
	vm.values[float64(16)] = p
	vm.install()
	if err := vm.runQueue(20000); err != nil {
		return "", err
	}
	if vm.result == "" {
		return "", fmt.Errorf("sentinel turnstile VM returned no token")
	}
	return vm.result, nil
}

func decodeSentinelDX(dx, key string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(dx, "="))
	}
	if err != nil {
		return nil, fmt.Errorf("decode sentinel dx: %w", err)
	}
	return []byte(xorSentinelString(string(decoded), key)), nil
}

func xorSentinelString(value, key string) string {
	if key == "" {
		return value
	}
	left := []rune(value)
	right := []rune(key)
	out := make([]rune, len(left))
	for index, item := range left {
		out[index] = item ^ right[index%len(right)]
	}
	return string(out)
}

type sentinelCallable func(args ...any) any

type sentinelOrderedMap struct {
	keys   []string
	values map[string]any
}

type sentinelVM struct {
	values    map[any]any
	startTime time.Time
	result    string
}

func (vm *sentinelVM) install() {
	vm.values[float64(1)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			vm.values[args[0]] = xorSentinelString(vm.toString(vm.get(args[0])), vm.toString(vm.get(args[1])))
		}
		return nil
	})
	vm.values[float64(2)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			vm.values[args[0]] = args[1]
		}
		return nil
	})
	vm.values[float64(3)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			vm.result = base64.StdEncoding.EncodeToString([]byte(vm.toString(args[0])))
		}
		return nil
	})
	vm.values[float64(5)] = sentinelCallable(func(args ...any) any {
		if len(args) < 2 {
			return nil
		}
		current, incoming := vm.get(args[0]), vm.get(args[1])
		switch typed := current.(type) {
		case []any:
			vm.values[args[0]] = append(append([]any{}, typed...), incoming)
		case string:
			vm.values[args[0]] = typed + vm.toString(incoming)
		case float64:
			vm.values[args[0]] = vm.toString(current) + vm.toString(incoming)
		default:
			if _, ok := incoming.(string); ok {
				vm.values[args[0]] = vm.toString(current) + vm.toString(incoming)
			} else {
				vm.values[args[0]] = "NaN"
			}
		}
		return nil
	})
	vm.values[float64(6)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 3 {
			vm.values[args[0]] = vm.property(vm.get(args[1]), vm.get(args[2]))
		}
		return nil
	})
	vm.values[float64(7)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			values := make([]any, 0, len(args)-1)
			for _, arg := range args[1:] {
				values = append(values, vm.get(arg))
			}
			vm.callTarget(vm.get(args[0]), values)
		}
		return nil
	})
	vm.values[float64(8)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			vm.values[args[0]] = vm.values[args[1]]
		}
		return nil
	})
	vm.values[float64(11)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			vm.values[args[0]] = nil
		}
		return nil
	})
	vm.values[float64(12)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			vm.values[args[0]] = vm.values
		}
		return nil
	})
	vm.values[float64(13)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			values := append([]any{}, args[2:]...)
			if _, err := vm.callTargetChecked(vm.get(args[1]), values); err != nil {
				vm.values[args[0]] = err.Error()
			}
		}
		return nil
	})
	vm.values[float64(14)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			var value any
			if json.Unmarshal([]byte(vm.toString(vm.values[args[1]])), &value) == nil {
				vm.values[args[0]] = value
			}
		}
		return nil
	})
	vm.values[float64(15)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			vm.values[args[0]] = sentinelJSON(vm.values[args[1]])
		}
		return nil
	})
	vm.values[float64(17)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			values := make([]any, 0, len(args)-2)
			for _, arg := range args[2:] {
				values = append(values, vm.get(arg))
			}
			vm.values[args[0]] = vm.callTarget(vm.get(args[1]), values)
		}
		return nil
	})
	vm.values[float64(18)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			raw, err := base64.StdEncoding.DecodeString(vm.toString(vm.values[args[0]]))
			if err == nil {
				vm.values[args[0]] = string(raw)
			}
		}
		return nil
	})
	vm.values[float64(19)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 1 {
			vm.values[args[0]] = base64.StdEncoding.EncodeToString([]byte(vm.toString(vm.values[args[0]])))
		}
		return nil
	})
	vm.values[float64(20)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 3 && equalSentinel(vm.get(args[0]), vm.get(args[1])) {
			vm.callTarget(vm.get(args[2]), args[3:]...)
		}
		return nil
	})
	vm.values[float64(21)] = sentinelCallable(func(args ...any) any {
		if len(args) < 4 {
			return nil
		}
		delta := numberSentinel(vm.get(args[0])) - numberSentinel(vm.get(args[1]))
		if math.Abs(delta) > math.Abs(numberSentinel(vm.get(args[2]))) {
			vm.callTarget(vm.get(args[3]), args[4:]...)
		}
		return nil
	})
	vm.values[float64(22)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			previous := vm.queue()
			vm.values[float64(9)] = toAnyList(args[1])
			_ = vm.runQueue(20000)
			vm.values[args[0]] = "None"
			vm.values[float64(9)] = previous
		}
		return nil
	})
	vm.values[float64(23)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 && vm.get(args[0]) != nil {
			if callable, ok := vm.get(args[1]).(sentinelCallable); ok {
				callable(args[2:]...)
			}
		}
		return nil
	})
	vm.values[float64(24)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 3 {
			vm.values[args[0]] = vm.property(vm.get(args[1]), vm.get(args[2]))
		}
		return nil
	})
	vm.values[float64(25)] = sentinelCallable(func(...any) any { return nil })
	vm.values[float64(26)] = sentinelCallable(func(...any) any { return nil })
	vm.values[float64(27)] = sentinelCallable(func(args ...any) any {
		if len(args) < 2 {
			return nil
		}
		current, incoming := vm.get(args[0]), vm.get(args[1])
		if list, ok := current.([]any); ok {
			for index, value := range list {
				if equalSentinel(value, incoming) {
					vm.values[args[0]] = append(list[:index], list[index+1:]...)
					break
				}
			}
			return nil
		}
		vm.values[args[0]] = numberSentinel(current) - numberSentinel(incoming)
		return nil
	})
	vm.values[float64(28)] = sentinelCallable(func(...any) any { return nil })
	vm.values[float64(29)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 3 {
			vm.values[args[0]] = numberSentinel(vm.get(args[1])) < numberSentinel(vm.get(args[2]))
		}
		return nil
	})
	vm.values[float64(30)] = sentinelCallable(func(args ...any) any {
		if len(args) < 3 {
			return nil
		}
		if captured, ok := args[3].([]any); ok {
			queue := append([]any{}, captured...)
			captureKeys := toAnyList(args[2])
			vm.values[args[0]] = sentinelCallable(func(callArgs ...any) any {
				previous := vm.queue()
				for index, key := range captureKeys {
					if index < len(callArgs) {
						vm.values[key] = callArgs[index]
					}
				}
				vm.values[float64(9)] = append([]any{}, queue...)
				_ = vm.runQueue(20000)
				vm.values[float64(9)] = previous
				return nil
			})
		} else {
			queue := toAnyList(args[2])
			vm.values[args[0]] = sentinelCallable(func(callArgs ...any) any {
				_ = callArgs
				previous := vm.queue()
				vm.values[float64(9)] = append([]any{}, queue...)
				_ = vm.runQueue(20000)
				vm.values[float64(9)] = previous
				return nil
			})
		}
		return nil
	})
	vm.values[float64(33)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 3 {
			vm.values[args[0]] = numberSentinel(vm.get(args[1])) * numberSentinel(vm.get(args[2]))
		}
		return nil
	})
	vm.values[float64(34)] = sentinelCallable(func(args ...any) any {
		if len(args) >= 2 {
			vm.values[args[0]] = vm.get(args[1])
		}
		return nil
	})
}

func (vm *sentinelVM) runQueue(limit int) error {
	for steps := 0; ; steps++ {
		queue := vm.queue()
		if len(queue) == 0 {
			return nil
		}
		if steps > limit {
			return fmt.Errorf("sentinel_turnstile_vm_step_limit")
		}
		token := toAnyList(queue[0])
		vm.values[float64(9)] = queue[1:]
		if len(token) == 0 {
			continue
		}
		callable, ok := vm.values[token[0]].(sentinelCallable)
		if !ok {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			_ = callable(token[1:]...)
		}()
	}
}

func (vm *sentinelVM) queue() []any {
	return toAnyList(vm.values[float64(9)])
}

func (vm *sentinelVM) get(key any) any {
	if value, ok := vm.values[key]; ok {
		return value
	}
	return nil
}

func (vm *sentinelVM) toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case float64:
		if typed == math.Trunc(typed) {
			return strconv.FormatFloat(typed, 'f', 1, 64)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(value)
	}
}

func (vm *sentinelVM) property(object, key any) any {
	if ordered, ok := object.(*sentinelOrderedMap); ok {
		return ordered.values[vm.toString(key)]
	}
	if values, ok := object.(map[string]any); ok {
		return values[vm.toString(key)]
	}
	if values, ok := object.([]any); ok {
		index := int(numberSentinel(key))
		if index >= 0 && index < len(values) {
			return values[index]
		}
		return nil
	}
	if text, ok := object.(string); ok {
		if text == "window.document" && vm.toString(key) == "location" {
			return "https://chatgpt.com/"
		}
		if key := vm.toString(key); key != "undefined" && key != "None" {
			return text + "." + key
		}
	}
	return nil
}

func (vm *sentinelVM) callTarget(target any, args ...any) any {
	value, _ := vm.callTargetChecked(target, args)
	return value
}

func (vm *sentinelVM) callTargetChecked(target any, args []any) (any, error) {
	if callable, ok := target.(sentinelCallable); ok {
		return callable(args...), nil
	}
	name, _ := target.(string)
	switch name {
	case "window.performance.now":
		return float64(time.Since(vm.startTime).Microseconds())/1000 + rand.Float64(), nil
	case "window.Object.create":
		return &sentinelOrderedMap{values: map[string]any{}}, nil
	case "window.Object.keys":
		if len(args) > 0 {
			switch typed := args[0].(type) {
			case string:
				if typed == "window.localStorage" {
					return []any{"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4", "STATSIG_LOCAL_STORAGE_STABLE_ID", "client-correlated-secret", "oai/apps/capExpiresAt", "oai-did", "STATSIG_LOCAL_STORAGE_LOGGING_REQUEST", "UiState.isNavigationCollapsed.1"}, nil
				}
			case *sentinelOrderedMap:
				result := make([]any, 0, len(typed.keys))
				for _, key := range typed.keys {
					result = append(result, key)
				}
				return result, nil
			case map[string]any:
				result := make([]any, 0, len(typed))
				for key := range typed {
					result = append(result, key)
				}
				return result, nil
			}
		}
	case "window.Reflect.set":
		if len(args) >= 3 {
			key := vm.toString(args[1])
			switch typed := args[0].(type) {
			case *sentinelOrderedMap:
				if _, exists := typed.values[key]; !exists {
					typed.keys = append(typed.keys, key)
				}
				typed.values[key] = args[2]
				return true, nil
			case map[string]any:
				typed[key] = args[2]
				return true, nil
			}
		}
		return false, nil
	}
	return nil, nil
}

func toAnyList(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

func numberSentinel(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case string:
		value, _ := strconv.ParseFloat(typed, 64)
		return value
	default:
		return 0
	}
}

func equalSentinel(left, right any) bool {
	switch a := left.(type) {
	case float64:
		return numberSentinel(right) == a
	default:
		return fmt.Sprint(left) == fmt.Sprint(right)
	}
}

func sentinelJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(raw)
}

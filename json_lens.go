package main

import (
	"encoding/json"
	"errors"
	"iter"
)

type JSONLens interface {
	At(path ...any) (JSONLens, bool)
	Set(v any) error
	AsString() (string, bool)
	AsInt() (int64, bool)
	AsFloat() (float64, bool)
	AsBool() (bool, bool)
	IsNull() bool
	AsArray() ([]JSONLens, bool)
	AsArrayIter() (iter.Seq[JSONLens], bool)
	Bytes() []byte
}

var errJSONLensPathNotFound = errors.New("json lens path not found")

type lensRoot struct {
	root *json.RawMessage
	path []any
}

type lensPointer struct {
	raw   json.RawMessage
	write func(json.RawMessage) error
}

func NewJSONLens(raw []byte) (JSONLens, error) {
	var root json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	return &lensRoot{root: &root}, nil
}

func (jl *lensRoot) At(path ...any) (JSONLens, bool) {
	nextPath := make([]any, 0, len(jl.path)+len(path))
	nextPath = append(nextPath, jl.path...)
	for _, step := range path {
		switch step.(type) {
		case string, int:
			nextPath = append(nextPath, step)
		default:
			return nil, false
		}
	}
	if _, ok := jl.resolvePath(nextPath); !ok {
		return nil, false
	}
	return &lensRoot{root: jl.root, path: nextPath}, true
}

func (jl *lensRoot) Set(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rewritten, err := jl.replacePath(*jl.root, jl.path, raw)
	if err != nil {
		return err
	}
	*jl.root = rewritten
	return nil
}

func (jl *lensRoot) AsString() (string, bool) { return asString(jl.resolve()) }
func (jl *lensRoot) AsInt() (int64, bool)     { return asInt(jl.resolve()) }
func (jl *lensRoot) AsFloat() (float64, bool) { return asFloat(jl.resolve()) }
func (jl *lensRoot) AsBool() (bool, bool)     { return asBool(jl.resolve()) }

func (jl *lensRoot) IsNull() bool {
	raw, ok := jl.resolve()
	return ok && string(raw) == "null"
}

func (jl *lensRoot) AsArray() ([]JSONLens, bool) {
	raw, ok := jl.resolve()
	return asArray(raw, ok, func(index int, item json.RawMessage) JSONLens {
		return pointerFromArray(item, func(next json.RawMessage) error {
			rewritten, err := jl.replacePath(*jl.root, append(append([]any{}, jl.path...), index), next)
			if err != nil {
				return err
			}
			*jl.root = rewritten
			return nil
		})
	})
}

func (jl *lensRoot) AsArrayIter() (iter.Seq[JSONLens], bool) {
	raw, ok := jl.resolve()
	return arrayIter(raw, ok, func(index int, item json.RawMessage) JSONLens {
		return pointerFromArray(item, func(next json.RawMessage) error {
			rewritten, err := jl.replacePath(*jl.root, append(append([]any{}, jl.path...), index), next)
			if err != nil {
				return err
			}
			*jl.root = rewritten
			return nil
		})
	})
}

func (jl *lensRoot) Bytes() []byte {
	return append([]byte(nil), (*jl.root)...)
}

func (jl *lensRoot) resolve() (json.RawMessage, bool) {
	return jl.resolvePath(jl.path)
}

func (jl *lensRoot) resolvePath(path []any) (json.RawMessage, bool) {
	current := *jl.root
	for _, step := range path {
		switch step := step.(type) {
		case string:
			var object map[string]json.RawMessage
			if err := json.Unmarshal(current, &object); err != nil {
				return nil, false
			}
			next, ok := object[step]
			if !ok {
				return nil, false
			}
			current = next
		case int:
			var array []json.RawMessage
			if err := json.Unmarshal(current, &array); err != nil || step < 0 || step >= len(array) {
				return nil, false
			}
			current = array[step]
		default:
			return nil, false
		}
	}
	return current, true
}

func (jl *lensRoot) replacePath(current json.RawMessage, path []any, next json.RawMessage) (json.RawMessage, error) {
	if len(path) == 0 {
		return append(json.RawMessage(nil), next...), nil
	}

	switch step := path[0].(type) {
	case string:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil {
			return nil, err
		}
		child, ok := object[step]
		if !ok {
			return nil, errJSONLensPathNotFound
		}
		rewrittenChild, err := jl.replacePath(child, path[1:], next)
		if err != nil {
			return nil, err
		}
		object[step] = rewrittenChild
		return json.Marshal(object)
	case int:
		var array []json.RawMessage
		if err := json.Unmarshal(current, &array); err != nil {
			return nil, err
		}
		if step < 0 || step >= len(array) {
			return nil, errJSONLensPathNotFound
		}
		rewrittenChild, err := jl.replacePath(array[step], path[1:], next)
		if err != nil {
			return nil, err
		}
		array[step] = rewrittenChild
		return json.Marshal(array)
	default:
		return nil, errJSONLensPathNotFound
	}
}

func (jl *lensPointer) At(path ...any) (JSONLens, bool) {
	current := jl
	for _, step := range path {
		next, ok := current.at(step)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func (jl *lensPointer) at(step any) (*lensPointer, bool) {
	switch step := step.(type) {
	case string:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(jl.raw, &object); err != nil {
			return nil, false
		}
		child, ok := object[step]
		if !ok {
			return nil, false
		}
		return &lensPointer{
			raw: child,
			write: func(next json.RawMessage) error {
				object[step] = append(json.RawMessage(nil), next...)
				rewritten, err := json.Marshal(object)
				if err != nil {
					return err
				}
				jl.raw = rewritten
				return jl.write(rewritten)
			},
		}, true
	case int:
		var array []json.RawMessage
		if err := json.Unmarshal(jl.raw, &array); err != nil || step < 0 || step >= len(array) {
			return nil, false
		}
		child := array[step]
		return &lensPointer{
			raw: child,
			write: func(next json.RawMessage) error {
				array[step] = append(json.RawMessage(nil), next...)
				rewritten, err := json.Marshal(array)
				if err != nil {
					return err
				}
				jl.raw = rewritten
				return jl.write(rewritten)
			},
		}, true
	default:
		return nil, false
	}
}

func (jl *lensPointer) Set(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	jl.raw = append(jl.raw[:0], raw...)
	return jl.write(jl.raw)
}

func (jl *lensPointer) AsString() (string, bool) { return asString(jl.raw, true) }
func (jl *lensPointer) AsInt() (int64, bool)     { return asInt(jl.raw, true) }
func (jl *lensPointer) AsFloat() (float64, bool) { return asFloat(jl.raw, true) }
func (jl *lensPointer) AsBool() (bool, bool)     { return asBool(jl.raw, true) }

func (jl *lensPointer) IsNull() bool {
	return string(jl.raw) == "null"
}

func (jl *lensPointer) AsArray() ([]JSONLens, bool) {
	return asArray(jl.raw, true, func(index int, item json.RawMessage) JSONLens {
		return pointerFromArray(item, func(next json.RawMessage) error {
			var array []json.RawMessage
			if err := json.Unmarshal(jl.raw, &array); err != nil {
				return err
			}
			if index < 0 || index >= len(array) {
				return errJSONLensPathNotFound
			}
			array[index] = append(json.RawMessage(nil), next...)
			rewritten, err := json.Marshal(array)
			if err != nil {
				return err
			}
			jl.raw = rewritten
			return jl.write(rewritten)
		})
	})
}

func (jl *lensPointer) AsArrayIter() (iter.Seq[JSONLens], bool) {
	return arrayIter(jl.raw, true, func(index int, item json.RawMessage) JSONLens {
		return pointerFromArray(item, func(next json.RawMessage) error {
			var array []json.RawMessage
			if err := json.Unmarshal(jl.raw, &array); err != nil {
				return err
			}
			if index < 0 || index >= len(array) {
				return errJSONLensPathNotFound
			}
			array[index] = append(json.RawMessage(nil), next...)
			rewritten, err := json.Marshal(array)
			if err != nil {
				return err
			}
			jl.raw = rewritten
			return jl.write(rewritten)
		})
	})
}

func (jl *lensPointer) Bytes() []byte {
	return append([]byte(nil), jl.raw...)
}

func pointerFromArray(raw json.RawMessage, write func(json.RawMessage) error) JSONLens {
	return &lensPointer{raw: append(json.RawMessage(nil), raw...), write: write}
}

func asString(raw json.RawMessage, ok bool) (string, bool) {
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func asInt(raw json.RawMessage, ok bool) (int64, bool) {
	if !ok {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func asFloat(raw json.RawMessage, ok bool) (float64, bool) {
	if !ok {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func asBool(raw json.RawMessage, ok bool) (bool, bool) {
	if !ok {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func asArray(raw json.RawMessage, ok bool, makeLens func(int, json.RawMessage) JSONLens) ([]JSONLens, bool) {
	if !ok {
		return nil, false
	}
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil, false
	}
	items := make([]JSONLens, len(array))
	for i, item := range array {
		items[i] = makeLens(i, item)
	}
	return items, true
}

func arrayIter(raw json.RawMessage, ok bool, makeLens func(int, json.RawMessage) JSONLens) (iter.Seq[JSONLens], bool) {
	if !ok {
		return nil, false
	}
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil, false
	}
	return func(yield func(JSONLens) bool) {
		for i, item := range array {
			if !yield(makeLens(i, item)) {
				return
			}
		}
	}, true
}

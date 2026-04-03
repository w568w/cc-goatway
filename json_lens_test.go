package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

const benchmarkJSONLensPayload = `{
	"metadata": {
		"user_id": "{\"device_id\":\"old-device\",\"account_uuid\":\"acct-1\",\"session_id\":\"sess-1\"}",
		"other": "keep"
	},
	"system": [
		{"type": "text", "text": "Platform: linux\nWorking directory: /home/alice/project"},
		"cc_version=2.1.81.a1b"
	],
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "hello"}, {"type": "text", "text": "world"}]},
		{"role": "assistant", "content": "done"}
	]
}`

func TestNewJSONLensRejectsInvalidJSON(t *testing.T) {
	if _, err := NewJSONLens([]byte(`{"broken"`)); err == nil {
		t.Fatal("expected invalid JSON to fail")
	}
}

func TestJSONLensAtSupportsMixedObjectAndArrayPaths(t *testing.T) {
	jl := mustJSONLens(t, `{
		"metadata": {"user_id": "abc"},
		"messages": [
			{"content": [{"type": "text", "text": "hello"}]}
		]
	}`)

	userID, ok := jl.At("metadata", "user_id")
	if !ok {
		t.Fatal("expected metadata.user_id path")
	}
	if got, ok := userID.AsString(); !ok || got != "abc" {
		t.Fatalf("unexpected user_id: %q %v", got, ok)
	}

	text, ok := jl.At("messages", 0, "content", 0, "text")
	if !ok {
		t.Fatal("expected nested text path")
	}
	if got, ok := text.AsString(); !ok || got != "hello" {
		t.Fatalf("unexpected text: %q %v", got, ok)
	}
	if _, ok := jl.At("messages", 1); ok {
		t.Fatal("expected out-of-range array index to fail")
	}
	if _, ok := jl.At("messages", -1); ok {
		t.Fatal("expected negative array index to fail")
	}
	if _, ok := jl.At("metadata", 0); ok {
		t.Fatal("expected object accessed by index to fail")
	}
	if _, ok := jl.At("metadata", true); ok {
		t.Fatal("expected unsupported path type to fail")
	}
}

func TestJSONLensScalarReadersAndTypeMismatch(t *testing.T) {
	jl := mustJSONLens(t, `{
		"string": "value",
		"int": 42,
		"float": 3.14,
		"bool": true,
		"null": null,
		"object": {"x": 1}
	}`)

	assertString(t, jl, []any{"string"}, "value")
	assertInt(t, jl, []any{"int"}, 42)
	assertFloat(t, jl, []any{"float"}, 3.14)
	assertBool(t, jl, []any{"bool"}, true)

	nullValue, ok := jl.At("null")
	if !ok || !nullValue.IsNull() {
		t.Fatal("expected null value")
	}
	if got, ok := nullValue.AsString(); !ok || got != "" {
		t.Fatalf("expected null string read to decode to zero value, got %q %v", got, ok)
	}

	objectValue, ok := jl.At("object")
	if !ok {
		t.Fatal("expected object value")
	}
	if _, ok := objectValue.AsBool(); ok {
		t.Fatal("expected object bool read to fail")
	}
	if objectValue.IsNull() {
		t.Fatal("object should not be null")
	}
}

func TestJSONLensSetPreservesUnrelatedFields(t *testing.T) {
	jl := mustJSONLens(t, `{
		"metadata": {"user_id": "old", "keep": 1},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hello", "extra": true}]}],
		"top": "keep"
	}`)

	userID, ok := jl.At("metadata", "user_id")
	if !ok {
		t.Fatal("expected metadata.user_id")
	}
	if err := userID.Set("new"); err != nil {
		t.Fatalf("set user_id: %v", err)
	}

	text, ok := jl.At("messages", 0, "content", 0, "text")
	if !ok {
		t.Fatal("expected nested text")
	}
	if err := text.Set("rewritten"); err != nil {
		t.Fatalf("set text: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(jl.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal rewritten json: %v", err)
	}

	metadata := got["metadata"].(map[string]any)
	if metadata["user_id"] != "new" || metadata["keep"].(float64) != 1 {
		t.Fatalf("unexpected metadata after set: %#v", metadata)
	}

	message := got["messages"].([]any)[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("expected role to be preserved: %#v", message)
	}
	block := message["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || block["extra"] != true || block["text"] != "rewritten" {
		t.Fatalf("unexpected block after set: %#v", block)
	}
	if got["top"] != "keep" {
		t.Fatalf("expected top-level field to be preserved: %#v", got)
	}
}

func TestJSONLensSetArrayElementByPath(t *testing.T) {
	jl := mustJSONLens(t, `{"items":["a","b","c"]}`)

	item, ok := jl.At("items", 1)
	if !ok {
		t.Fatal("expected array element path")
	}
	if err := item.Set("rewritten"); err != nil {
		t.Fatalf("set array element: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(jl.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal rewritten json: %v", err)
	}
	items := got["items"].([]any)
	if !reflect.DeepEqual(items, []any{"a", "rewritten", "c"}) {
		t.Fatalf("unexpected items after set: %#v", items)
	}
}

func TestJSONLensSetArrayElementViaAsArray(t *testing.T) {
	jl := mustJSONLens(t, `{"items":[{"v":"a"},{"v":"b"},{"v":"c"}]}`)

	items, ok := jl.At("items")
	if !ok {
		t.Fatal("expected items path")
	}
	array, ok := items.AsArray()
	if !ok || len(array) != 3 {
		t.Fatalf("unexpected AsArray result: %v %d", ok, len(array))
	}

	second, ok := array[1].At("v")
	if !ok {
		t.Fatal("expected nested v field")
	}
	if err := second.Set("rewritten"); err != nil {
		t.Fatalf("set nested array element field: %v", err)
	}

	assertString(t, jl, []any{"items", 0, "v"}, "a")
	assertString(t, jl, []any{"items", 1, "v"}, "rewritten")
	assertString(t, jl, []any{"items", 2, "v"}, "c")
}

func TestJSONLensSetArrayElementViaAsArrayIter(t *testing.T) {
	jl := mustJSONLens(t, `{"items":[1,2,3]}`)

	items, ok := jl.At("items")
	if !ok {
		t.Fatal("expected items path")
	}
	seq, ok := items.AsArrayIter()
	if !ok {
		t.Fatal("expected array iter")
	}

	index := 0
	for item := range seq {
		if index == 1 {
			if err := item.Set(int64(20)); err != nil {
				t.Fatalf("set iter array element: %v", err)
			}
		}
		index++
	}

	assertInt(t, jl, []any{"items", 0}, 1)
	assertInt(t, jl, []any{"items", 1}, 20)
	assertInt(t, jl, []any{"items", 2}, 3)
}

func TestJSONLensSetRootReplacesWholeDocument(t *testing.T) {
	jl := mustJSONLens(t, `{"a":1}`)
	if err := jl.Set(map[string]any{"b": 2}); err != nil {
		t.Fatalf("set root: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(jl.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal rewritten root: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"b": float64(2)}) {
		t.Fatalf("unexpected root after set: %#v", got)
	}
}

func TestJSONLensAsArrayAndIter(t *testing.T) {
	jl := mustJSONLens(t, `{"items":[{"v":1},{"v":2},{"v":3}]}`)

	items, ok := jl.At("items")
	if !ok {
		t.Fatal("expected items path")
	}

	array, ok := items.AsArray()
	if !ok || len(array) != 3 {
		t.Fatalf("unexpected AsArray result: %v %d", ok, len(array))
	}
	assertInt(t, array[0], []any{"v"}, 1)

	values := make([]int64, 0, 3)
	seq, ok := items.AsArrayIter()
	if !ok {
		t.Fatal("expected array iter")
	}
	for item := range seq {
		value, ok := item.At("v")
		if !ok {
			t.Fatal("expected v field")
		}
		got, ok := value.AsInt()
		if !ok {
			t.Fatal("expected int field")
		}
		values = append(values, got)
	}
	if !reflect.DeepEqual(values, []int64{1, 2, 3}) {
		t.Fatalf("unexpected iter values: %#v", values)
	}

	stopped := 0
	seq, ok = items.AsArrayIter()
	if !ok {
		t.Fatal("expected array iter second time")
	}
	for item := range seq {
		stopped++
		if stopped == 2 {
			break
		}
		if _, ok := item.At("v"); !ok {
			t.Fatal("expected early iteration item")
		}
	}
	if stopped != 2 {
		t.Fatalf("expected early stop at 2, got %d", stopped)
	}
}

func TestJSONLensArrayAPIsRejectNonArrays(t *testing.T) {
	jl := mustJSONLens(t, `{"value":{"nested":1}}`)
	value, ok := jl.At("value")
	if !ok {
		t.Fatal("expected value path")
	}
	if _, ok := value.AsArray(); ok {
		t.Fatal("expected AsArray on object to fail")
	}
	if _, ok := value.AsArrayIter(); ok {
		t.Fatal("expected AsArrayIter on object to fail")
	}
}

func TestJSONLensBytesReturnsCopy(t *testing.T) {
	jl := mustJSONLens(t, `{"a":1}`)
	first := jl.Bytes()
	first[0] = '['
	second := jl.Bytes()
	if string(second) != `{"a":1}` {
		t.Fatalf("expected Bytes to return copy, got %s", second)
	}
}

func BenchmarkJSONLensAt(b *testing.B) {
	jl := mustJSONLens(b, benchmarkJSONLensPayload)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		lens, ok := jl.At("messages", 0, "content", 1, "text")
		if !ok {
			b.Fatal("expected path")
		}
		if _, ok := lens.AsString(); !ok {
			b.Fatal("expected string")
		}
	}
}

func BenchmarkJSONLensSetNestedField(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		jl := mustJSONLens(b, benchmarkJSONLensPayload)
		lens, ok := jl.At("messages", 0, "content", 1, "text")
		if !ok {
			b.Fatal("expected path")
		}
		if err := lens.Set("rewritten"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONLensAsArrayIter(b *testing.B) {
	jl := mustJSONLens(b, benchmarkJSONLensPayload)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		messages, ok := jl.At("messages")
		if !ok {
			b.Fatal("expected messages")
		}
		seq, ok := messages.AsArrayIter()
		if !ok {
			b.Fatal("expected array iter")
		}
		count := 0
		for item := range seq {
			count++
			if _, ok := item.At("content"); !ok {
				b.Fatal("expected content")
			}
		}
		if count != 2 {
			b.Fatalf("unexpected item count: %d", count)
		}
	}
}

func mustJSONLens(tb testing.TB, raw string) JSONLens {
	tb.Helper()
	jl, err := NewJSONLens([]byte(raw))
	if err != nil {
		tb.Fatalf("NewJSONLens(%s): %v", raw, err)
	}
	return jl
}

func assertString(t *testing.T, jl JSONLens, path []any, want string) {
	t.Helper()
	target := jl
	if len(path) > 0 {
		var ok bool
		target, ok = jl.At(path...)
		if !ok {
			t.Fatalf("path not found: %#v", path)
		}
	}
	got, ok := target.AsString()
	if !ok || got != want {
		t.Fatalf("unexpected string at %#v: %q %v, want %q", path, got, ok, want)
	}
}

func assertInt(t *testing.T, jl JSONLens, path []any, want int64) {
	t.Helper()
	target := jl
	if len(path) > 0 {
		var ok bool
		target, ok = jl.At(path...)
		if !ok {
			t.Fatalf("path not found: %#v", path)
		}
	}
	got, ok := target.AsInt()
	if !ok || got != want {
		t.Fatalf("unexpected int at %#v: %d %v, want %d", path, got, ok, want)
	}
}

func assertFloat(t *testing.T, jl JSONLens, path []any, want float64) {
	t.Helper()
	target := jl
	if len(path) > 0 {
		var ok bool
		target, ok = jl.At(path...)
		if !ok {
			t.Fatalf("path not found: %#v", path)
		}
	}
	got, ok := target.AsFloat()
	if !ok || got != want {
		t.Fatalf("unexpected float at %#v: %f %v, want %f", path, got, ok, want)
	}
}

func assertBool(t *testing.T, jl JSONLens, path []any, want bool) {
	t.Helper()
	target := jl
	if len(path) > 0 {
		var ok bool
		target, ok = jl.At(path...)
		if !ok {
			t.Fatalf("path not found: %#v", path)
		}
	}
	got, ok := target.AsBool()
	if !ok || got != want {
		t.Fatalf("unexpected bool at %#v: %v %v, want %v", path, got, ok, want)
	}
}

package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file pins the **capability boundary** of the generic Webhook template.
//
// This is the only place in the package where a user-provided string is evaluated as code,
// so what it can and cannot do must be explicit and fixed by tests — otherwise someone
// could casually add a method to the template context or a readFile to the FuncMap,
// silently expanding the capability surface while the diff looks like a harmless helper.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can read a field or call a method). So if
// the template context can reach **any** type with exported methods, those methods are
// effectively exposed to the template author. The context here is deliberately pure data
// (exported fields only, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem. Before
// allowing it, consider carefully whether that method could be used by a template to read
// something that should not be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, which grants their capability to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal pins the set of functions exposed to templates.
//
// Every extra function in the FuncMap is an extra capability. Currently there are only
// json / jsons, which serialize values into JSON fragments — no file reads, no requests,
// no command execution.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, want %v. Before adding a function, confirm it does not expand the capability surface (no file reads/writes, no network requests, no command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in templates:
// accessing something non-existent must fail rather than echo anything, and the failure
// must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a non-existent field should be an error")
	}
	// The error must not include real content from the template context (finding title or
	// summary).
	for _, leak := range []string{"SQL injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a bad template is a configuration error that
// retries will not heal. If classified as retryable, one bad template would waste three
// backoff rounds on every delivery.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be caught at save time")
	}
	// Even if validation is bypassed and delivery attempted, it must be a permanent failure
	// rather than being retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "template render results must
// be valid JSON". It also blocks uses such as generating plain text from a template to
// trigger another protocol.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering to non-JSON must be rejected (rather than sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering to non-JSON should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error should state it is a JSON problem, got %v", err)
	}
}

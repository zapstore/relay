package config

import (
	"bufio"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// steroidKeys are read by steroid inside the relay process, not by a config struct.
var steroidKeys = []string{
	"LLM_URL", "LLM_API_KEY", "LLM_MODEL",
	"LLM_FALLBACK_URL", "LLM_FALLBACK_API_KEY", "LLM_FALLBACK_MODEL",
	"BLOSSOM_URL", "LEAF_MODEL_DIR", "ONNXRUNTIME_LIB",
}

var exampleKey = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=`)

// TestEnvExample keeps .env.example and the env tags equal: every tag has a
// line, and every line is a tag or a steroid key.
func TestEnvExample(t *testing.T) {
	f, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var example []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		if m := exampleKey.FindStringSubmatch(scan.Text()); m != nil {
			example = append(example, m[1])
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}

	tags := envTags(reflect.TypeOf(Config{}))
	for _, tag := range tags {
		if !slices.Contains(example, tag) {
			t.Errorf(".env.example is missing %s", tag)
		}
	}
	for _, key := range example {
		if !slices.Contains(tags, key) && !slices.Contains(steroidKeys, key) {
			t.Errorf(".env.example has %s, which nothing reads", key)
		}
	}
}

func envTags(typ reflect.Type) []string {
	var tags []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		if tag, ok := field.Tag.Lookup("env"); ok {
			name, _, _ := strings.Cut(tag, ",")
			tags = append(tags, name)
			continue
		}
		if field.Type.Kind() == reflect.Struct {
			tags = append(tags, envTags(field.Type)...)
		}
	}
	return tags
}

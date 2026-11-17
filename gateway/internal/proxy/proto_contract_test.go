package proxy

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	protoComment = regexp.MustCompile(`//[^\n]*`)
	protoMessage = regexp.MustCompile(`(?s)message\s+(\w+)\s*\{(.*?)\n\}`)
	protoField   = regexp.MustCompile(`(?m)^\s*(?:map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*;`)
)

// protoFields returns the field names of each message in tool.proto.
func protoFields(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../proto/tool.proto")
	if err != nil {
		t.Fatalf("read tool.proto: %v", err)
	}
	src := protoComment.ReplaceAllString(string(raw), "")
	out := map[string][]string{}
	for _, m := range protoMessage.FindAllStringSubmatch(src, -1) {
		for _, f := range protoField.FindAllStringSubmatch(m[2], -1) {
			out[m[1]] = append(out[m[1]], f[1])
		}
		sort.Strings(out[m[1]])
	}
	return out
}

func jsonTags(v any) []string {
	rt := reflect.TypeOf(v)
	tags := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		if name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			tags = append(tags, name)
		}
	}
	sort.Strings(tags)
	return tags
}

// TestProtoContractMatchesJSONStructs guards the frozen worker contract: the
// field names in tool.proto must equal the JSON tags on the gateway's structs
// (Connect JSON uses the proto field names as written).
func TestProtoContractMatchesJSONStructs(t *testing.T) {
	fields := protoFields(t)
	cases := map[string]any{
		"InvokeRequest":  InvokeRequest{},
		"InvokeResponse": InvokeResponse{},
		"Error":          InvokeError{}, // proto message is Error; Go type is InvokeError
	}
	for msg, v := range cases {
		want, got := fields[msg], jsonTags(v)
		if len(want) == 0 {
			t.Fatalf("message %s not found in tool.proto", msg)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: proto fields %v != struct json tags %v", msg, want, got)
		}
	}
}

// Adapted from github.com/pocketbase/pocketbase/plugins/jsvm's mapper_test.go.
// Copyright (c) 2022 - present, Gani Georgiev. Distributed under the MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
//
// Modifications: upstream tests through FieldMapper.FieldName /
// FieldMapper.MethodName; Soda removes the goja-specific FieldMapper
// so these tests call convertGoToJSName directly with the same inputs.
// Copyright (c) 2026 - present, Yasushi Itoh.

package soda

import (
	"testing"
)

func TestConvertGoToJSName(t *testing.T) {
	scenarios := []struct {
		name     string
		expected string
	}{
		{"", ""},
		{"test", "test"},
		{"Test", "test"},
		{"miXeD", "miXeD"},
		{"MiXeD", "miXeD"},
		{"ResolveRequestAsJSON", "resolveRequestAsJSON"},
		{"Variable_with_underscore", "variable_with_underscore"},
		{"ALLCAPS", "allcaps"},
		{"ALL_CAPS_WITH_UNDERSCORE", "all_caps_with_underscore"},
		{"OIDCMap", "oidcMap"},
		{"MD5", "md5"},
		{"OAuth2", "oauth2"},
		{"JSONField", "jsonField"},
		{"GetField", "getField"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := convertGoToJSName(s.name)
			if result != s.expected {
				t.Fatalf("Expected %q for %q, got %q", s.expected, s.name, result)
			}
		})
	}
}

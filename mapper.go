// This file is adapted from github.com/pocketbase/pocketbase/plugins/jsvm.
// Copyright (c) 2022 - present, Gani Georgiev. Distributed under the MIT License.
// https://github.com/pocketbase/pocketbase/blob/master/LICENSE.md
//
// Modifications: the upstream FieldMapper struct (which implements the
// goja.FieldNameMapper interface) is removed because Soda's Ramune
// engine does not consume it. convertGoToJSName and nameExceptions are
// retained verbatim so the JS property names Soda emits match the ones
// upstream pb_hooks authors already rely on.
// Copyright (c) 2026 - present, Yasushi Itoh.

package soda

import (
	"strings"
	"unicode"
)

var nameExceptions = map[string]string{"OAuth2": "oauth2"}

func convertGoToJSName(name string) string {
	if v, ok := nameExceptions[name]; ok {
		return v
	}

	startUppercase := make([]rune, 0, len(name))

	for _, c := range name {
		if c != '_' && !unicode.IsUpper(c) && !unicode.IsDigit(c) {
			break
		}

		startUppercase = append(startUppercase, c)
	}

	totalStartUppercase := len(startUppercase)

	// all uppercase eg. "JSON" -> "json"
	if len(name) == totalStartUppercase {
		return strings.ToLower(name)
	}

	// eg. "JSONField" -> "jsonField"
	if totalStartUppercase > 1 {
		return strings.ToLower(name[0:totalStartUppercase-1]) + name[totalStartUppercase-1:]
	}

	// eg. "GetField" -> "getField"
	if totalStartUppercase == 1 {
		return strings.ToLower(name[0:1]) + name[1:]
	}

	return name
}

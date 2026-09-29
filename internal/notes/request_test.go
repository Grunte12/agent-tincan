package notes

import (
	"slices"
	"strings"
	"testing"
)

func TestParseStructuredValidForms(t *testing.T) {
	id := "0F8E2A4C-1B3D-4E5F-8A9B-0C1D2E3F4A5B"
	for body, want := range map[string]Request{
		`note: {"op":"add","title":"Tent","body":"blue","tags":["camping","gear"]}`: {Op: OpAdd, Title: "Tent", Body: "blue", Tags: []string{"camping", "gear"}},
		"note:\n{\"op\":\"add\",\"title\":\"Tent\"}":                                {Op: OpAdd, Title: "Tent"},
		`NOTE: {"op":"search","query":"tent"}`:                                      {Op: OpSearch, Query: "tent", Count: DefaultSearchCount},
		`note: {"op":"search","query":"tent","count":20}`:                           {Op: OpSearch, Query: "tent", Count: 20},
		`note: {"op":"read","id":"` + id + `"}`:                                     {Op: OpRead, ID: id},
	} {
		raw, ok := structuredBody(body)
		if !ok {
			t.Fatalf("%q not seen as structured", body)
		}
		got, err := ParseStructured(raw)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got.Op != want.Op || got.Title != want.Title || got.Body != want.Body || !slices.Equal(got.Tags, want.Tags) ||
			got.Query != want.Query || got.Count != want.Count || got.ID != want.ID {
			t.Fatalf("%q: got %+v, want %+v", body, got, want)
		}
	}
}

func TestStructuredBodyNeedsMarker(t *testing.T) {
	for _, body := range []string{"Note: save my tent idea", "save this: x", "notes: {}", ""} {
		if _, ok := structuredBody(body); ok {
			t.Errorf("%q seen as structured", body)
		}
	}
}

func TestParseStructuredRejects(t *testing.T) {
	for _, raw := range []string{
		`{"op":"add","title":"x","query":"y"}`,
		`{"op":"search","query":"x","count":21}`,
		`{"op":"search","query":"x","count":-1}`,
		`{"op":"search","query":""}`,
		`{"op":"read","id":"not-a-uuid"}`,
		`{"op":"read","id":"0F8E2A4C-1B3D-4E5F-8A9B-0C1D2E3F4A5B","title":"x"}`,
		`{"op":"add","body":"no title"}`,
		`{"op":"add","title":"x","tags":null}`,
		`{"op":"add","title":"x"}{"op":"add","title":"y"}`,
		`null`,
		`[]`,
	} {
		if r, err := ParseStructured(raw); err == nil {
			t.Errorf("%s: parsed as %+v", raw, r)
		}
	}
}

func TestValidateAdd(t *testing.T) {
	ok := Request{Op: OpAdd, Title: "Tent\twith tab", Body: "any\nbody\tis fine", Tags: []string{"a", "b"}}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid add rejected: %v", err)
	}
	for name, r := range map[string]Request{
		"newline title":   {Op: OpAdd, Title: "a\nb"},
		"blank title":     {Op: OpAdd, Title: "  "},
		"long title":      {Op: OpAdd, Title: strings.Repeat("x", maxTitleRunes+1)},
		"comma tag":       {Op: OpAdd, Title: "x", Tags: []string{"a,b"}},
		"control tag":     {Op: OpAdd, Title: "x", Tags: []string{"a\x01"}},
		"blank tag":       {Op: OpAdd, Title: "x", Tags: []string{" "}},
		"too many tags":   {Op: OpAdd, Title: "x", Tags: slices.Repeat([]string{"t"}, maxTags+1)},
		"long body":       {Op: OpAdd, Title: "x", Body: strings.Repeat("x", maxBodyBytes+1)},
		"NUL in body":     {Op: OpAdd, Title: "x", Body: "a\x00b"},
		"unknown op":      {Op: "delete"},
		"search no query": {Op: OpSearch},
	} {
		if err := Validate(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

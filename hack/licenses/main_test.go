package main

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"MIT License\n\nPermission is hereby granted, free of charge, to any person":    "MIT",
		"Apache License\nVersion 2.0, January 2004":                                     "Apache-2.0",
		"Redistribution and use in source and binary forms ... Neither the name of":     "BSD-3-Clause",
		"Redistribution and use in source and binary forms, with or without":            "BSD-2-Clause",
		"ISC License\nPermission to use, copy, modify, and/or distribute this software": "ISC",
		"Mozilla Public License Version 2.0":                                            "MPL-2.0",
		"GNU GENERAL PUBLIC LICENSE Version 3":                                          "GPL",
		"GNU AFFERO GENERAL PUBLIC LICENSE":                                             "AGPL",
		"GNU LESSER GENERAL PUBLIC LICENSE":                                             "LGPL",
		"This is free and unencumbered software released into the public domain.":       "Unlicense",
		"all rights reserved": "unknown",
	}
	for text, want := range cases {
		if got := Classify(text); got != want {
			t.Errorf("Classify(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestDisallowedAndRender(t *testing.T) {
	deps := []Dep{{"b", "v1", "MIT"}, {"a", "v2", "GPL"}, {"c", "1.0", "(MIT OR GPL-3.0)"}, {"d", "1", "unknown"}}
	bad := Disallowed(deps)
	if len(bad) != 2 || bad[0].Name != "a" || bad[1].Name != "d" {
		t.Fatalf("bad = %+v", bad)
	}
	doc := Render("test", deps)
	if !strings.Contains(doc, "| a | v2 | GPL |") || strings.Index(doc, "| a |") > strings.Index(doc, "| b |") {
		t.Fatal(doc)
	}
}

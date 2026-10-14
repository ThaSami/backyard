package config

import _ "embed"

// Example is the annotated example configuration printed by
// `backyard config example`.
//
//go:embed example.yaml
var Example string

package api

import _ "embed"

// OpenAPI is the source contract served by the companion itself.
//
//go:embed openapi.yaml
var OpenAPI []byte

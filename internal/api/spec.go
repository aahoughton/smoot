package api

import (
	_ "embed"
	"slices"
)

//go:embed openapi.yaml
var openapiSpec []byte

// OpenAPISpec returns the embedded OpenAPI document.
func OpenAPISpec() []byte { return openapiSpec }

// Routes returns "METHOD /pattern" for every API route, including those
// registered only when attachments are enabled.
func Routes() []string {
	var out []string
	for _, rt := range append(slices.Clip(routes), attachmentRoutes...) {
		out = append(out, rt.method+" "+rt.pattern)
	}
	return out
}

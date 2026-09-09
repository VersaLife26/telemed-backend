package gateway

import (
	_ "embed"
	"net/http"
)

// openapiYAML is a byte-for-byte copy of the repository root's openapi.yaml,
// embedded because go:embed cannot reach outside its own package directory.
// Keep the two in sync -- `make openapi-sync` does the copy.
//
//go:embed openapi.embed.yaml
var openapiYAML []byte

// ServeOpenAPI serves the aggregated OpenAPI 3.1 document describing every
// route the gateway exposes. This is what the TypeScript Next.js teams
// generate their clients from.
func ServeOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(openapiYAML)
}

// docsHTML renders a self-contained Swagger UI pointed at /openapi.yaml. The
// Swagger UI bundle itself loads from a CDN (unpkg) -- the only external
// asset this service fetches at request time, and only when a human opens
// /docs in a browser, never on the API request path.
const docsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>Telemed API Gateway - Docs</title>
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>body { margin: 0; } #swagger-ui { max-width: 100%; }</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: '/openapi.yaml',
        dom_id: '#swagger-ui',
        presets: [SwaggerUIBundle.presets.apis],
        layout: 'BaseLayout',
      });
    };
  </script>
</body>
</html>`

// ServeDocsUI serves the Swagger UI shell at /docs.
func ServeDocsUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}

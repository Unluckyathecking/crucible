package openapi

import (
	"encoding/json"
	"log"
	"net/http"
)

// --- handler -----------------------------------------------------------------

// Handler returns an http.HandlerFunc that serves the OpenAPI document built from invokeRoutes.
// The document is built eagerly at construction time so invalid descriptors panic at server
// startup rather than on the first request. No DB or Redis access.
func Handler(invokeRoutes []RouteDescriptor) http.HandlerFunc {
	// Defensive copy: the caller may mutate elements of the original slice after
	// Handler returns. make+copy gives the closure its own backing array, isolating
	// it from such mutations.
	routes := make([]RouteDescriptor, len(invokeRoutes))
	copy(routes, invokeRoutes)
	doc := Build(routes)
	// GET /v1/usage is framework infra (see routes.go), not a per-product invoke
	// route, so it is layered onto the served document here rather than added to
	// Build()'s static paths: Build() is also called directly by
	// server.TestV1RoutesDriftGuard, which asserts every /v1/* path it produces
	// (other than /v1/billing/*) is POST-only — the invariant that keeps
	// per-product routes and their documentation from drifting apart. Merging
	// here documents the endpoint in the actual served /openapi.json without
	// perturbing that invoke-route-only guarantee.
	//
	// Set only Get on whatever PathItem is already there rather than replacing
	// it outright: a product clone could name an invoke route "/usage" (POST
	// /v1/usage, routed independently through the per-product V1Routes block —
	// chi dispatches by method, so it coexists fine with this GET at runtime),
	// and a wholesale overwrite here would silently drop that POST operation
	// from the served document.
	usageItem := doc.Paths["/v1/usage"]
	usageItem.Get = usagePathItem().Get
	doc.Paths["/v1/usage"] = usageItem

	// Layer the webhook-endpoint management routes on for the same reason and
	// in the same manner as /v1/usage above: framework infra, not per-product
	// invoke routes, so they must stay out of Build()'s invoke-route-only paths
	// (see server.TestV1RoutesDriftGuard) but should still appear in the
	// document actually served to API consumers.
	mergePathItems(doc.Paths, webhookEndpointsPathItems())

	// Layer the customer API-key self-management routes on for the same
	// reason as webhookEndpointsPathItems above.
	mergePathItems(doc.Paths, keysPathItems())

	// Layer the customer error-history self-service route on for the same
	// reason as usagePathItem/keysPathItems above.
	mergePathItems(doc.Paths, errorsPathItems())

	// Layer the async job status-polling route on for the same reason as
	// usagePathItem/keysPathItems/errorsPathItems above: framework infra
	// (gateway/internal/jobs), not a per-product invoke route — kept out of
	// Build()'s invoke-route-only paths (see server.TestV1RoutesDriftGuard)
	// but still documented in the actual served /openapi.json.
	mergePathItems(doc.Paths, jobsPathItems())

	// Layer the per-event usage export route on for the same reason as
	// usagePathItem/keysPathItems/errorsPathItems/jobsPathItems above:
	// framework infra (gateway/internal/selfusagedetail), not a per-product
	// invoke route — kept out of Build()'s invoke-route-only paths (see
	// server.TestV1RoutesDriftGuard) but still documented in the actual served
	// /openapi.json.
	mergePathItems(doc.Paths, usageEventsPathItems())

	// Layer the webhook delivery log route on for the same reason as
	// usagePathItem/keysPathItems/errorsPathItems/jobsPathItems/usageEventsPathItems
	// above: framework infra (gateway/internal/webhookout), not a per-product
	// invoke route — kept out of Build()'s invoke-route-only paths (see
	// server.TestV1RoutesDriftGuard) but still documented in the actual served
	// /openapi.json.
	mergePathItems(doc.Paths, webhookDeliveriesPathItems())

	b, err := json.Marshal(doc)
	if err != nil {
		panic("openapi: failed to marshal static document: " + err.Error())
	}
	body := b
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		if _, err := w.Write(body); err != nil {
			log.Printf("openapi: write: %v", err)
		}
	}
}

// mergePathItems copies the operations (Get, Post, Patch, Delete) each item
// defines onto the entry already in paths, rather than replacing the whole
// PathItem, so operations set earlier for the same path are kept.
func mergePathItems(paths, items map[string]PathItem) {
	for path, item := range items {
		existing := paths[path]
		if item.Get != nil {
			existing.Get = item.Get
		}
		if item.Post != nil {
			existing.Post = item.Post
		}
		if item.Patch != nil {
			existing.Patch = item.Patch
		}
		if item.Delete != nil {
			existing.Delete = item.Delete
		}
		paths[path] = existing
	}
}

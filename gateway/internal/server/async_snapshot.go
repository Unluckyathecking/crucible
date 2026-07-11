package server

import "github.com/Unluckyathecking/crucible/gateway/internal/openapi"

// AnnotatedRoutes returns a snapshot of V1Routes with Async=true applied to
// every route that appears in AsyncRoutes. This is the same snapshot NewRouter
// serves via /openapi.json and spec-dump renders to clients/openapi.json —
// keeping both callers on this function is what prevents the two documents
// from diverging when a clone opts a route into AsyncRoutes.
func AnnotatedRoutes() []openapi.RouteDescriptor {
	routes := make([]openapi.RouteDescriptor, len(V1Routes))
	copy(routes, V1Routes)
	for i := range routes {
		if _, async := AsyncRoutes[routes[i].Path]; async {
			routes[i].Async = true
		}
	}
	return routes
}

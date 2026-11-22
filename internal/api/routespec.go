// Package api implements the hdr.toys Remote Library HTTP API v1 with a
// fail-closed declarative RouteSpec registry.
package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/thebigsasha/cloudedits/internal/auth"
)

// RouteSpec declaratively binds every HTTP endpoint to its required auth
// policy, fine-grained permission, resource scope, and p95 latency SLO budget.
type RouteSpec struct {
	Method        string
	Pattern       string
	Auth          auth.AuthPolicy
	Permission    auth.Permission
	ResourceScope auth.ResourceScope
	SLOBudgetMs   int
	Handler       http.HandlerFunc
}

// ValidateRoutes enforces fail-closed invariants across the entire route table:
//   - Every route MUST declare Method, Pattern, Handler, AuthPolicy, and SLOBudgetMs > 0.
//   - Every AuthAuthenticated route MUST declare an explicit Permission.
//   - AuthDevOnly routes are strictly forbidden unless allowDevOnly is true
//     (compiled with //go:build dev_apis).
func ValidateRoutes(routes []RouteSpec, allowDevOnly bool) error {
	if len(routes) == 0 {
		return errors.New("route table is empty")
	}
	seen := make(map[string]bool, len(routes))
	for i, r := range routes {
		if r.Method == "" || r.Pattern == "" || r.Handler == nil {
			return fmt.Errorf("route[%d] missing method, pattern, or handler", i)
		}
		key := r.Method + " " + r.Pattern
		if seen[key] {
			return fmt.Errorf("duplicate route %q", key)
		}
		seen[key] = true

		switch r.Auth {
		case auth.AuthUnset:
			return fmt.Errorf("route %q omits AuthPolicy (fail-closed)", key)
		case auth.AuthPublic:
			// Public routes do not require a Permission.
		case auth.AuthAuthenticated:
			if r.Permission == auth.PermNone {
				return fmt.Errorf("authenticated route %q omits Permission (fail-closed)", key)
			}
		case auth.AuthDevOnly:
			if !allowDevOnly {
				return fmt.Errorf("dev-only route %q registered outside dev_apis build (fail-closed)", key)
			}
		default:
			return fmt.Errorf("route %q has unknown AuthPolicy %d", key, r.Auth)
		}

		if r.SLOBudgetMs <= 0 {
			return fmt.Errorf("route %q omits positive SLOBudgetMs", key)
		}
	}
	return nil
}

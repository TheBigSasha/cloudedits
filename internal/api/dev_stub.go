//go:build !dev_apis

package api

import (
	"context"
	"io"
	"net/http"
)

const extraModeEnabled = false

func (s *Server) extraRoutes() []RouteSpec {
	return nil
}

func (s *Server) extraCaps(_ map[string]any) {}

func (s *Server) preHandle(_ http.ResponseWriter, _ *http.Request) bool {
	return false
}

func (s *Server) wrapStream(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	return &ctxReadSeeker{ctx: ctx, rs: rs}
}

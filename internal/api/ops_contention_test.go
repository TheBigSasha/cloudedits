package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

// An update abandoned because another process kept rewriting the sidecar is
// retryable. Answering 400 would tell the client its ops are malformed, and it
// would stop sending them.
func TestApplyFailureStatusSeparatesContentionFromBadRequests(t *testing.T) {
	contended := httptest.NewRecorder()
	writeApplyFailure(contended, fmt.Errorf("update sidecar for x: %w", sidecar.ErrContended))
	if contended.Code != http.StatusServiceUnavailable || contended.Header().Get("Retry-After") == "" {
		t.Fatalf("contention: status=%d Retry-After=%q, want 503 with Retry-After", contended.Code, contended.Header().Get("Retry-After"))
	}
	other := httptest.NewRecorder()
	writeApplyFailure(other, errors.New("malformed"))
	if other.Code != http.StatusBadRequest {
		t.Fatalf("other failure: status=%d, want 400", other.Code)
	}
}

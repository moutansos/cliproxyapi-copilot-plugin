package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestUpstreamStatusErrorMapsPaymentRequiredToQuota(t *testing.T) {
	t.Parallel()

	var statusErr *StatusError
	if !errors.As(upstreamStatusError(http.StatusPaymentRequired, "credits exhausted"), &statusErr) {
		t.Fatal("upstreamStatusError() did not return *StatusError")
	}
	if statusErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want %d", statusErr.HTTPStatus, http.StatusTooManyRequests)
	}
	if statusErr.Code != "quota_exhausted" || !statusErr.Retryable {
		t.Fatalf("Code/Retryable = %q/%v, want quota_exhausted/true", statusErr.Code, statusErr.Retryable)
	}
	if !strings.Contains(statusErr.Message, "HTTP 402") {
		t.Fatalf("Message = %q, want the original upstream status kept", statusErr.Message)
	}
}

func TestUpstreamStatusErrorKeepsOtherStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway} {
		var statusErr *StatusError
		if !errors.As(upstreamStatusError(status, ""), &statusErr) {
			t.Fatalf("status %d: not a *StatusError", status)
		}
		if statusErr.HTTPStatus != status || statusErr.Code != "upstream_error" {
			t.Fatalf("status %d: got HTTPStatus %d code %q", status, statusErr.HTTPStatus, statusErr.Code)
		}
	}
}

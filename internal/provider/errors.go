package provider

import (
	"fmt"
	"net/http"
)

type StatusError struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func statusError(code, message string, status int) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &StatusError{Code: code, Message: message, HTTPStatus: status}
}

func upstreamStatusError(status int, detail string) error {
	message := fmt.Sprintf("Copilot upstream returned HTTP %d", status)
	if detail != "" {
		message += ": " + detail
	}
	code := "upstream_error"
	hostStatus := status
	if status == http.StatusPaymentRequired {
		// GitHub answers 402 when the account's AI credit allowance is exhausted.
		// Report it to the host as 429 so the scheduler applies its quota cooldown
		// to this credential instead of a short payment-required model pause.
		code = "quota_exhausted"
		hostStatus = http.StatusTooManyRequests
	}
	return &StatusError{
		Code:       code,
		Message:    message,
		HTTPStatus: hostStatus,
		Retryable:  hostStatus == http.StatusRequestTimeout || hostStatus == http.StatusTooManyRequests || hostStatus >= 500,
	}
}

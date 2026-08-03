package thunder

import (
	"errors"
	"fmt"
)

// APIError is returned when Central responds with a non-2xx status.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string
	ErrorType  string `json:"error"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("thunder central API %s %s returned %d %s: %s", e.Method, e.Path, e.StatusCode, e.ErrorType, e.Message)
	}
	if e.ErrorType != "" {
		return fmt.Sprintf("thunder central API %s %s returned %d %s", e.Method, e.Path, e.StatusCode, e.ErrorType)
	}
	if e.Status != "" {
		return fmt.Sprintf("thunder central API %s %s returned %s", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("thunder central API %s %s returned %d", e.Method, e.Path, e.StatusCode)
}

func (e *APIError) IsUnauthorized() bool { return e.StatusCode == 401 }
func (e *APIError) IsForbidden() bool    { return e.StatusCode == 403 }
func (e *APIError) IsNotFound() bool     { return e.StatusCode == 404 }
func (e *APIError) IsConflict() bool     { return e.StatusCode == 409 }

// IsPermanent reports whether retrying the same request should not help.
func (e *APIError) IsPermanent() bool {
	return e.StatusCode == 400 || e.StatusCode == 401 || e.StatusCode == 403 || e.StatusCode == 404 || e.StatusCode == 409
}

func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsUnauthorized()
}

func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsForbidden()
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsNotFound()
}

func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsConflict()
}

func IsPermanent(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsPermanent()
}

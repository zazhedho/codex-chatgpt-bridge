package domain

import "errors"

var (
	ErrNotFound              = errors.New("resource not found")
	ErrNotAuthorized         = errors.New("resource is not authorized")
	ErrConflict              = errors.New("resource state conflict")
	ErrInvalidInput          = errors.New("invalid input")
	ErrQueueFull             = errors.New("task queue is full")
	ErrServiceUnavailable    = errors.New("service unavailable")
	ErrInvalidTaskTransition = errors.New("invalid task state transition")
)

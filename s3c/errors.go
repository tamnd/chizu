package s3c

import (
	"encoding/xml"
	"errors"
	"fmt"
)

// The retry taxonomy from spec 2107 doc 02 section 3: 412 means the caller
// lost a CAS race and must re-read to learn who won; 404 is definitive
// absence; 409, 429, and 5xx are retried internally with backoff; a transport
// failure on a write that may have reached the server is ambiguous, because
// s3c cannot know whether the bytes landed, and only the caller (the chain,
// via writer ids) can resolve authorship.
var (
	// ErrNotFound reports a 404: the object does not exist.
	ErrNotFound = errors.New("s3c: not found")
	// ErrPrecondition reports a 412: an If-Match or If-None-Match condition
	// failed, meaning the caller lost a CAS race and should re-read.
	ErrPrecondition = errors.New("s3c: precondition failed")
)

// AmbiguousError reports a write whose outcome is unknown: the request may or
// may not have reached the server before the transport failed. s3c never
// retries these, because a blind retry of a CAS-create that actually landed
// would misreport the caller as having lost its own race. Callers re-read and
// decide by authorship.
type AmbiguousError struct {
	Op  string
	Key string
	Err error
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("s3c: %s %q outcome ambiguous: %v", e.Op, e.Key, e.Err)
}

func (e *AmbiguousError) Unwrap() error { return e.Err }

// APIError is a non-retryable rejection from the service that is not covered
// by a sentinel above.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("s3c: http %d %s: %s", e.Status, e.Code, e.Message)
}

type xmlError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func apiError(status int, body []byte) *APIError {
	var xe xmlError
	_ = xml.Unmarshal(body, &xe)
	return &APIError{Status: status, Code: xe.Code, Message: xe.Message}
}

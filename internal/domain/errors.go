package domain

import "errors"

// Domain errors. Check for them with errors.Is.
// The HTTP layer will map them to S3 error responses.
var (
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrInvalidKey        = errors.New("invalid object key")
	ErrInvalidMetadata   = errors.New("invalid metadata")
	ErrMetadataTooLarge  = errors.New("metadata too large")
	ErrInvalidToken      = errors.New("invalid continuation token")

	ErrBucketExists   = errors.New("bucket already exists")
	ErrBucketNotEmpty = errors.New("bucket not empty")
	ErrNoSuchBucket   = errors.New("no such bucket")
	ErrNoSuchKey      = errors.New("no such key")
	ErrNoSuchUpload   = errors.New("no such upload")
)

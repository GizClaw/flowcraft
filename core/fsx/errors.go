package fsx

import "errors"

// ErrDenied reports that a write was refused.
//
// It is returned when the kernel refuses the write (a read-only mount, a
// seatbelt deny rule, an ACL). It is never returned for a read: reads are
// unrestricted by decision, so there is no read to refuse.
var ErrDenied = errors.New("fsx: write refused")

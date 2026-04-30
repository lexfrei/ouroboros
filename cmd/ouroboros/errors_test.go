package main

import "github.com/cockroachdb/errors"

// errFakeNamespace is the static error tests use to simulate a downward-API
// read failure. err113 forbids inline errors.New in tests.
var errFakeNamespace = errors.New("downward API namespace file unreadable")

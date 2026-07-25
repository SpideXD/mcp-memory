package main

import (
	"errors"
	"fmt"
)

var (
	errBadStatus      = errors.New("bad status")
	errTimeout        = errors.New("health check timeout")
)

func errModelNotFound(p string) error { return fmt.Errorf("model not found: %s", p) }

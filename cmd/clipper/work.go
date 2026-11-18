package main

import (
	"context"
	"errors"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// errWorkNotBuilt is what clipper work answers until the job loop exists.
var errWorkNotBuilt = errors.New("the work loop is not built yet (issue #34); use clipper render to render a clip by hand")

// runWork will run the job loop (#34). It already loads the full clipper
// configuration, which requires pogfarm_url and api_secret, and reports its
// problems together with the missing loop.
func runWork(_ context.Context, configPath string, _ env) error {
	if _, err := config.LoadClipper(configPath); err != nil {
		return errors.Join(errWorkNotBuilt, err)
	}
	return errWorkNotBuilt
}

// SPDX-License-Identifier: Apache-2.0

package managedfields_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// k8s is a client against a bare envtest apiserver, shared by the package.
//
// Booted here rather than through ctrltest.Suite on purpose: this package must
// stay importable by a consumer's package-level envtests without the suite.
var k8s client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		ControlPlaneStartTimeout: 120 * time.Second,
		ControlPlaneStopTimeout:  60 * time.Second,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		os.Exit(1)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		_ = env.Stop()
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"net/http"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// InvalidPatch returns interceptor.Funcs whose Patch fails with the error the
// apiserver returns for a failed JSON-patch op, for every patch of an object
// match accepts, and passes every other call through unchanged. times > 0 limits it
// to that many failures (then it passes through); times <= 0 means always.
//
// This fakes the error rather than provoking it. A real stale resourceVersion
// cannot be forced on demand: the test would have to race a second writer
// against the patch and win every time. The real apiserver also returns 422
// Invalid for two different causes, a failed JSON-patch test op (stale
// resourceVersion, or a wrong manager name) and a wrong index into a list. The
// fake mirrors that shared error, so it cannot tell the two apart either, and a
// controller that reacts to it has to handle both the same way.
//
// Only Patch is intercepted, not SubResourcePatch. The consumer this serves
// patches the main resource with a JSON patch whose test op can fail; a status
// patch is a different call that no such test op guards, so failing it would
// exercise a path the apiserver does not produce.
//
// The error is built the way the apiserver builds it (k8s.io/apiserver's
// patch handler calls NewGenericServerResponse with 422 and an empty
// GroupResource and name), not with NewInvalid. So it satisfies
// apierrors.IsInvalid but carries no Details (no Kind, Name or Causes) and the
// generic client-side message, exactly as a controller sees it from envtest. A
// test that asserted on the object name or a cause would pass against NewInvalid
// and see nothing against the real server.
//
// The failure counter is atomic because concurrent reconciles share one
// interceptor, and a plain counter would let more than times of them fail.
func InvalidPatch(match func(client.Object) bool, times int) interceptor.Funcs {
	var failed atomic.Int64
	return interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			c client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			if !match(obj) {
				return c.Patch(ctx, obj, patch, opts...)
			}
			// Add before comparing so exactly times callers see a value
			// within the limit however they interleave.
			if times > 0 && failed.Add(1) > int64(times) {
				return c.Patch(ctx, obj, patch, opts...)
			}
			return apierrors.NewGenericServerResponse(
				http.StatusUnprocessableEntity,
				"",
				schema.GroupResource{},
				"",
				"testing value /metadata/resourceVersion failed: test failed",
				0,
				false,
			)
		},
	}
}

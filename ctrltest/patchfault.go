// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"context"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// InvalidPatch returns interceptor.Funcs whose Patch fails with the
// apiserver-shaped 422 Invalid error for every patch of an object match
// accepts, and passes every other call through unchanged. times > 0 limits it
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
			return apierrors.NewInvalid(
				groupKind(c, obj),
				obj.GetName(),
				field.ErrorList{field.Invalid(
					field.NewPath("patch"),
					string(patch.Type()),
					"the patch test operation failed",
				)},
			)
		},
	}
}

// groupKind resolves obj's GroupKind from the client's scheme, then from the
// object itself, and is empty when neither knows. An empty GroupKind still
// yields an error that satisfies apierrors.IsInvalid.
func groupKind(c client.Client, obj client.Object) schema.GroupKind {
	if gvk, err := apiutil.GVKForObject(obj, c.Scheme()); err == nil {
		return gvk.GroupKind()
	}
	return obj.GetObjectKind().GroupVersionKind().GroupKind()
}

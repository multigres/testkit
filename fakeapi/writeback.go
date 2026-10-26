// SPDX-License-Identifier: Apache-2.0

package fakeapi

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// createFunc and updateFunc leave the caller's object holding the generation
// the store holds. For an Unstructured or PartialObjectMetadata of a kind the
// scheme types, the fake converts the object to a typed copy before it reaches
// the tracker, so the generation the tracker sets lands on the copy. The fake
// copies resourceVersion back to the caller itself, and a real client decodes
// the whole response into the object it was given, so without this the object
// in hand would report a generation the store never had.
func createFunc(
	next func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error,
) func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		var err error
		if next != nil {
			err = next(ctx, c, obj, opts...)
		} else {
			err = c.Create(ctx, obj, opts...)
		}
		if err != nil {
			return err
		}
		co := &client.CreateOptions{}
		co.ApplyOptions(opts)
		if len(co.DryRun) > 0 {
			return nil
		}
		return writeBackGeneration(ctx, c, obj)
	}
}

func updateFunc(
	next func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error,
) func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
	return func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		var err error
		if next != nil {
			err = next(ctx, c, obj, opts...)
		} else {
			err = c.Update(ctx, obj, opts...)
		}
		if err != nil {
			return err
		}
		uo := &client.UpdateOptions{}
		uo.ApplyOptions(opts)
		if len(uo.DryRun) > 0 {
			return nil
		}
		return writeBackGeneration(ctx, c, obj)
	}
}

// writeBackGeneration copies the stored generation onto obj. Callers skip it for
// a dry-run write, which stores nothing and so leaves the caller's generation as
// controller-runtime's fake leaves it; reading back would hand a dry-run Create
// of a stored name that object's generation. It runs only after a write that
// succeeded, so an object the read-back cannot find leaves nothing to copy
// rather than an error: an Update that removes the last finalizer from an
// object being deleted completes the deletion, and another writer may delete
// the object in between. A real client reports neither as a failure of the
// write.
func writeBackGeneration(ctx context.Context, c client.WithWatch, obj client.Object) error {
	switch obj.(type) {
	case runtime.Unstructured, *metav1.PartialObjectMetadata:
	default:
		return nil
	}
	stored, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("copy of %T is not a client.Object", obj)
	}
	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, stored); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read back the generation of %s: %w", key, err)
	}
	obj.SetGeneration(stored.GetGeneration())
	return nil
}

// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/testkit/managedfields"
)

// The ownership assertions take an already fetched object: the caller reads,
// the assertion decides. Reading inside would hide which client and which
// moment the claim is about, and a field-ownership claim is only meaningful
// against one specific read.

// ownershipDecodes reports whether obj's managedFields decode, failing through
// c's mode (abort or collect) if not. Callers must return on false: Owners
// panics on undecodable input, and in collecting mode the failure does not
// stop the test, so falling through would turn a reported failure into a crash.
func ownershipDecodes(c *C, obj client.Object) bool {
	c.Helper()
	if _, err := managedfields.Entries(obj); err != nil {
		c.Fail(err.Error())
		return false
	}
	return true
}

func objName(obj client.Object) string {
	name := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}
	return name
}

// OwnedBy asserts that manager claims exactly path in obj's managedFields, on
// any subresource. path is the fieldpath form, such as
// `.spec.template.spec.containers[name="app"].image`.
func (c *C) OwnedBy(obj client.Object, manager, path string) {
	c.Helper()
	if !ownershipDecodes(c, obj) {
		return
	}
	owners := managedfields.Owners(obj, path)
	if !slices.Contains(owners, manager) {
		c.Fail(fmt.Sprintf("%s: %q does not own %s\nowners of that path: %q\nall managers: %q",
			objName(obj), manager, path, owners, managedfields.Managers(obj)))
	}
}

// NotOwned asserts that no manager claims exactly path in obj's managedFields.
func (c *C) NotOwned(obj client.Object, path string) {
	c.Helper()
	if !ownershipDecodes(c, obj) {
		return
	}
	if owners := managedfields.Owners(obj, path); len(owners) > 0 {
		c.Fail(fmt.Sprintf("%s: %s is owned by %q, want no owner", objName(obj), path, owners))
	}
}

// NoManager asserts that none of names appears as a manager in obj's
// managedFields, on any subresource.
func (c *C) NoManager(obj client.Object, names ...string) {
	c.Helper()
	if !ownershipDecodes(c, obj) {
		return
	}
	present := managedfields.Managers(obj)
	var found []string
	for _, n := range names {
		if slices.Contains(present, n) {
			found = append(found, n)
		}
	}
	slices.Sort(found)
	if len(found) > 0 {
		c.Fail(fmt.Sprintf("%s: managers %s still present, want none of them\nall managers: %q",
			objName(obj), strings.Join(quoteAll(found), ", "), present))
	}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = `"` + s + `"`
	}
	return out
}

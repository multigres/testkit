// SPDX-License-Identifier: Apache-2.0

// Package fakeapi builds controller-runtime fake clients that behave like a real
// API server in the two ways unit tests most often rely on: metadata.name and
// metadata.namespace field selectors, and metadata.generation.
//
// controller-runtime's fake refuses a List or Watch whose field selector names
// a field with no registered index, so client.MatchingFields{"metadata.name": n}
// fails against it although a real API server serves that selector natively.
// And nothing in the fake sets or bumps metadata.generation, so a controller
// that compares generation with status.observedGeneration sees 0 forever.
// [NewClientBuilder] closes both gaps and hands back the ordinary
// *fake.ClientBuilder.
//
// Watch honours those selectors too, which controller-runtime's fake does not:
// it passes a watch only the resource and namespace. fakeapi installs a Watch
// interceptor that filters the events. It also honours a field registered with
// [WithFieldIndex], and refuses a selector on any other field as List refuses a
// field with no registered index. An object that enters or leaves the selector
// because an indexed field changed is delivered as Added or Deleted, as a real
// server does. Callers add their own interceptors with
// [WithInterceptors] rather than WithInterceptorFuncs, which would replace it
// and the Create and Update wrappers that copy the stored generation back onto
// an Unstructured or PartialObjectMetadata the caller holds.
//
// What is modelled is deliberately narrow. Generation is set to 1 on create,
// whatever the client sent, except that objects seeded with WithObjects keep a
// non-zero generation they already carry, as fixture state. It is bumped by one
// on a write that changes anything but metadata and status, which is the rule a
// CRD with a status subresource follows, and by one when a deletion with
// finalizers begins. The bump happens inside the same tracker write as the
// change, so a watcher sees one event, not two.
//
// Where this knowingly differs from a real server:
//   - Every kind gets a generation. A real server leaves it 0 on kinds such as
//     ConfigMap and Secret, whose strategies never set it.
//   - A status change through the main Update on a kind registered without
//     WithStatusSubresource does not bump. A real CRD without the status
//     subresource does, since its strategy drops only metadata.
//   - Field selectors are equality only ("=" and "=="), as in the fake's List.
//     A real server also serves "!=".
//   - A watch starts from now, not from a resourceVersion, as in
//     controller-runtime's fake. A consumer that lists and then watches can
//     miss a change made between the two, including an object leaving its
//     selector, which a real server would replay as Deleted.
//   - Indexes cover the kinds in the scheme when [NewClientBuilder] runs. A
//     kind the fake first registers later, from an Unstructured of a kind the
//     scheme lacks, has none, so its field selectors are refused; register
//     the kind in the scheme before building.
//
// What is not modelled, because a unit test that leans on it is testing the
// fake rather than the code: defaulting, validation, admission, and
// managed-fields fidelity. Anything that depends on those belongs in ctrltest or
// against a real envtest API server, where the behaviour is computed by the real
// machinery rather than approximated.
package fakeapi

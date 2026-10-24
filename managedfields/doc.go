// SPDX-License-Identifier: Apache-2.0

// Package managedfields decodes and manipulates metadata.managedFields of real
// objects on a real apiserver.
//
// There is no simulation here and there must not be one. Field ownership is
// computed by the apiserver's structured-merge-diff machinery, and a model of
// it in a test helper would encode the author's belief about that machinery
// rather than its behaviour. This package therefore only reads what the server
// recorded (Entries, Managers, Owners) and drives the server to record what a
// test needs (Seed, RenameManager).
//
// It deliberately does not import ctrltest, so a consumer's package-level
// envtests can use it without adopting the suite.
package managedfields

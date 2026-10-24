// SPDX-License-Identifier: Apache-2.0

// Package etcdsim is an in-memory model of a multi-member etcd cluster, shaped
// to the maintenance calls the multigres-operator makes against a TopoServer:
// Status, Members, Health, MoveLeader and Defragment.
//
// It exists because the operator's maintenance flow refuses on specific
// conditions (a missing or learner member, members that disagree on the
// leader, an etcd alarm, an unhealthy member, a leadership transfer that does
// not take effect) and a real etcd under envtest is single-node. Membership
// and leadership-transfer checks therefore cannot be exercised there at all.
// A Cluster lets a test put the cluster into each of those states, and
// interleave a change in the middle of a step through Hook, deterministically.
//
// [Client] is satisfied structurally: it is a copy of the operator's
// maintenance client interface, whose method set is all that matters to Go. The
// operator hands a Cluster's Client to its reconciler through its own factory
// seam, so testkit never imports the operator.
//
// What is modelled, and what is not: a member's DBSize halves on Defragment
// while its in-use size stays fixed, so reclaimable space is reported
// consistently before and after, and a defragmented member does not look
// reclaimable again. Status does not fail for an unhealthy member, only Health
// and Defragment do, so a test can tell which probe a caller relied on. There
// is no raft, no quorum and no time.
package etcdsim

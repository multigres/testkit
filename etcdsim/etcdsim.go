// SPDX-License-Identifier: Apache-2.0

package etcdsim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Client is the maintenance surface of an etcd cluster that the operator
// consumes. It is copied from the operator's unexported interface so that a
// *Cluster client satisfies it without testkit importing the operator.
type Client interface {
	Status(context.Context, string) (*clientv3.StatusResponse, error)
	Members(context.Context) (*clientv3.MemberListResponse, error)
	Health(context.Context, string) error
	MoveLeader(context.Context, string, uint64) error
	Defragment(context.Context, string) error
	Close()
}

// clusterID is fixed: the operator only compares it across members, and a
// split cluster ID is not a fault this package models.
const clusterID uint64 = 0xe7cd0000c1

// Member describes one etcd member at construction time.
type Member struct {
	ID       uint64
	Name     string
	Endpoint string // the string passed to Status/Health/Defragment
	Learner  bool
	Healthy  bool
	DBSize   int64
	Alarms   []string // non-empty makes Status report an error alarm
}

type member struct {
	Member
	// inUse stays fixed across Defragment: defragmenting reclaims free space,
	// it does not change what is live. Reporting it lets a caller that computes
	// reclaimable space from DbSize-DbSizeInUse see it reach zero.
	inUse int64
	// view, when set, is the leader this member's Status reports instead of
	// the cluster's.
	view uint64
}

// Cluster is an in-memory etcd cluster. It is safe for concurrent use.
type Cluster struct {
	mu      sync.Mutex
	members map[string]*member // by Name
	leader  uint64
	stall   bool

	defragLog     []string
	moveLeaderLog []uint64
	failNext      map[string][]error
	hooks         map[string][]func(string)
}

// New returns a cluster of the given members. The first member is the
// initial leader.
func New(members ...Member) *Cluster {
	c := &Cluster{
		members:  make(map[string]*member, len(members)),
		failNext: make(map[string][]error),
		hooks:    make(map[string][]func(string)),
	}
	for i, m := range members {
		m.Alarms = append([]string(nil), m.Alarms...)
		c.members[m.Name] = &member{Member: m, inUse: m.DBSize / 2}
		if i == 0 {
			c.leader = m.ID
		}
	}
	return c
}

// NewHealthy returns n healthy members etcd-0..etcd-(n-1) with endpoints
// "etcd-<i>:2379" and a 1 MiB database each. etcd-0 leads.
func NewHealthy(n int) *Cluster {
	members := make([]Member, n)
	for i := range members {
		members[i] = Member{
			ID:       uint64(i + 1),
			Name:     fmt.Sprintf("etcd-%d", i),
			Endpoint: fmt.Sprintf("etcd-%d:2379", i),
			Healthy:  true,
			DBSize:   1 << 20,
		}
	}
	return New(members...)
}

// Client returns a Client viewing this cluster. Each call returns an
// independent client, so closing one does not affect another.
func (c *Cluster) Client() Client { return &client{c: c} }

// Leader returns the ID of the current leader.
func (c *Cluster) Leader() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leader
}

// DBSize returns the named member's database size.
func (c *Cluster) DBSize(name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mustMember(name).DBSize
}

// DefragLog returns the endpoints defragmented, in call order.
func (c *Cluster) DefragLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.defragLog...)
}

// MoveLeaderLog returns the target IDs of MoveLeader calls that were
// accepted, in call order, including stalled ones.
func (c *Cluster) MoveLeaderLog() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint64(nil), c.moveLeaderLog...)
}

// SetHealthy marks the named member healthy or not.
func (c *Cluster) SetHealthy(name string, healthy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mustMember(name).Healthy = healthy
}

// SetAlarm replaces the named member's alarms. No alarms clears them.
func (c *Cluster) SetAlarm(name string, alarms ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mustMember(name).Alarms = append([]string(nil), alarms...)
}

// SplitLeaderView makes only the named member's Status report a different
// leader, as a member that has not yet seen an election would.
func (c *Cluster) SplitLeaderView(name string, leader uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mustMember(name).view = leader
}

// RemoveMember drops the named member from the cluster, so Members no longer
// matches what the caller expects. If it led, the lowest-ID voting member
// takes over, since an etcd cluster is never leaderless by fiat.
func (c *Cluster) RemoveMember(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := c.mustMember(name)
	delete(c.members, name)
	if c.leader != removed.ID {
		return
	}
	c.leader = 0
	for _, m := range c.sorted() {
		if !m.Learner {
			c.leader = m.ID
			return
		}
	}
}

// StallLeaderMove makes MoveLeader return nil without changing the leader.
func (c *Cluster) StallLeaderMove(stall bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stall = stall
}

// FailNext makes the next call of method ("Status", "Members", "Health",
// "MoveLeader" or "Defragment") return err, once. Repeated calls queue.
func (c *Cluster) FailNext(method string, err error) {
	checkMethod(method)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext[method] = append(c.failNext[method], err)
}

// Hook registers fn to run before each call of method takes effect, with the
// endpoint the call named ("" for Members). It runs without the cluster lock
// held, so fn may call any Cluster fault method to interleave a change into
// the middle of a caller's step.
func (c *Cluster) Hook(method string, fn func(endpoint string)) {
	checkMethod(method)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks[method] = append(c.hooks[method], fn)
}

func checkMethod(method string) {
	switch method {
	case "Status", "Members", "Health", "MoveLeader", "Defragment":
	default:
		panic(fmt.Sprintf("etcdsim: unknown method %q", method))
	}
}

// mustMember panics on an unknown name: a fault aimed at a member that does
// not exist is a test bug, and silence would hide it.
func (c *Cluster) mustMember(name string) *member {
	m, ok := c.members[name]
	if !ok {
		panic(fmt.Sprintf("etcdsim: no member named %q", name))
	}
	return m
}

func (c *Cluster) sorted() []*member {
	out := make([]*member, 0, len(c.members))
	for _, m := range c.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (c *Cluster) byEndpoint(endpoint string) (*member, error) {
	for _, m := range c.members {
		if m.Endpoint == endpoint {
			return m, nil
		}
	}
	names := make([]string, 0, len(c.members))
	for _, m := range c.sorted() {
		names = append(names, m.Endpoint)
	}
	return nil, fmt.Errorf(
		"etcdsim: unknown endpoint %q (members: %s)",
		endpoint,
		strings.Join(names, ", "),
	)
}

// enter runs the shared prologue of every call: cancellation, hooks (without
// the lock), then a queued failure. It returns with c.mu held on success.
func (c *Cluster) enter(ctx context.Context, method, endpoint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	hooks := slices.Clone(c.hooks[method])
	c.mu.Unlock()
	for _, fn := range hooks {
		fn(endpoint)
	}
	c.mu.Lock()
	if q := c.failNext[method]; len(q) > 0 {
		err := q[0]
		c.failNext[method] = q[1:]
		c.mu.Unlock()
		return err
	}
	return nil
}

var errClosed = errors.New("etcdsim: client closed")

type client struct {
	c      *Cluster
	mu     sync.Mutex
	closed bool
}

func (cl *client) check() error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return errClosed
	}
	return nil
}

func (cl *client) Close() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.closed = true
}

func (cl *client) Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	if err := cl.check(); err != nil {
		return nil, err
	}
	c := cl.c
	if err := c.enter(ctx, "Status", endpoint); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	m, err := c.byEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	leader := c.leader
	if m.view != 0 {
		leader = m.view
	}
	return &clientv3.StatusResponse{
		Header:      &etcdserverpb.ResponseHeader{ClusterId: clusterID, MemberId: m.ID},
		Leader:      leader,
		DbSize:      m.DBSize,
		DbSizeInUse: min(m.inUse, m.DBSize),
		IsLearner:   m.Learner,
		Errors:      append([]string(nil), m.Alarms...),
	}, nil
}

func (cl *client) Members(ctx context.Context) (*clientv3.MemberListResponse, error) {
	if err := cl.check(); err != nil {
		return nil, err
	}
	c := cl.c
	if err := c.enter(ctx, "Members", ""); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	resp := &clientv3.MemberListResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: clusterID},
	}
	for _, m := range c.sorted() {
		resp.Members = append(resp.Members, &etcdserverpb.Member{
			ID:         m.ID,
			Name:       m.Name,
			IsLearner:  m.Learner,
			ClientURLs: []string{m.Endpoint},
		})
	}
	return resp, nil
}

func (cl *client) Health(ctx context.Context, endpoint string) error {
	if err := cl.check(); err != nil {
		return err
	}
	c := cl.c
	if err := c.enter(ctx, "Health", endpoint); err != nil {
		return err
	}
	defer c.mu.Unlock()
	m, err := c.byEndpoint(endpoint)
	if err != nil {
		return err
	}
	if !m.Healthy {
		return fmt.Errorf("etcdsim: member %s is unhealthy", m.Name)
	}
	return nil
}

func (cl *client) MoveLeader(ctx context.Context, endpoint string, id uint64) error {
	if err := cl.check(); err != nil {
		return err
	}
	c := cl.c
	if err := c.enter(ctx, "MoveLeader", endpoint); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if _, err := c.byEndpoint(endpoint); err != nil {
		return err
	}
	var target *member
	for _, m := range c.members {
		if m.ID == id {
			target = m
		}
	}
	if target == nil {
		return fmt.Errorf("etcdsim: move leader to unknown member id %d", id)
	}
	if target.Learner {
		return fmt.Errorf("etcdsim: move leader to learner %s", target.Name)
	}
	c.moveLeaderLog = append(c.moveLeaderLog, id)
	if !c.stall {
		c.leader = id
	}
	return nil
}

func (cl *client) Defragment(ctx context.Context, endpoint string) error {
	if err := cl.check(); err != nil {
		return err
	}
	c := cl.c
	if err := c.enter(ctx, "Defragment", endpoint); err != nil {
		return err
	}
	defer c.mu.Unlock()
	m, err := c.byEndpoint(endpoint)
	if err != nil {
		return err
	}
	if !m.Healthy {
		return fmt.Errorf("etcdsim: member %s is unhealthy", m.Name)
	}
	m.DBSize = max(m.DBSize/2, 1)
	c.defragLog = append(c.defragLog, endpoint)
	return nil
}

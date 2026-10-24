// SPDX-License-Identifier: Apache-2.0

package etcdsim

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var _ Client = (*client)(nil)

var bg = context.Background()

func status(t *testing.T, cl Client, ep string) (leader uint64, size int64) {
	t.Helper()
	s, err := cl.Status(bg, ep)
	if err != nil {
		t.Fatalf("Status(%s): %v", ep, err)
	}
	return s.Leader, s.DbSize
}

func TestDefragShrinksAndLogs(t *testing.T) {
	c := NewHealthy(3)
	cl := c.Client()
	if err := cl.Defragment(bg, "etcd-1:2379"); err != nil {
		t.Fatal(err)
	}
	if got := c.DBSize("etcd-1"); got != DefaultInUse {
		t.Fatalf("etcd-1 size = %d, want %d", got, DefaultInUse)
	}
	if got := c.DBSize("etcd-0"); got != DefaultDBSize {
		t.Fatalf("etcd-0 size = %d, want untouched", got)
	}
	if got := c.DefragLog(); !reflect.DeepEqual(got, []string{"etcd-1:2379"}) {
		t.Fatalf("DefragLog = %v", got)
	}
	s, _ := cl.Status(bg, "etcd-1:2379")
	if s.DbSize != DefaultInUse || s.DbSizeInUse != DefaultInUse {
		t.Fatalf(
			"after defrag size=%d inUse=%d, want both %d (nothing reclaimable)",
			s.DbSize,
			s.DbSizeInUse,
			DefaultInUse,
		)
	}
}

func TestDefragReportsReclaimableBeforehand(t *testing.T) {
	s, err := NewHealthy(1).Client().Status(bg, "etcd-0:2379")
	if err != nil {
		t.Fatal(err)
	}
	if s.DbSizeInUse <= 0 || s.DbSizeInUse >= s.DbSize {
		t.Fatalf("size=%d inUse=%d, want 0 < inUse < size", s.DbSize, s.DbSizeInUse)
	}
}

func TestDefragMinimumOne(t *testing.T) {
	c := New(Member{ID: 1, Name: "a", Endpoint: "a", Healthy: true, DBSize: 1})
	if err := c.Client().Defragment(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if got := c.DBSize("a"); got != 1 {
		t.Fatalf("size = %d, want 1", got)
	}
}

func TestDefragUnhealthyErrorsAndLogsNothing(t *testing.T) {
	c := NewHealthy(3)
	c.SetHealthy("etcd-2", false)
	if err := c.Client().Defragment(bg, "etcd-2:2379"); err == nil {
		t.Fatal("want error")
	}
	if got := c.DefragLog(); len(got) != 0 {
		t.Fatalf("DefragLog = %v, want empty", got)
	}
	if got := c.DBSize("etcd-2"); got != DefaultDBSize {
		t.Fatalf("size changed to %d", got)
	}
}

func TestHealth(t *testing.T) {
	c := NewHealthy(2)
	cl := c.Client()
	if err := cl.Health(bg, "etcd-1:2379"); err != nil {
		t.Fatal(err)
	}
	c.SetHealthy("etcd-1", false)
	if err := cl.Health(bg, "etcd-1:2379"); err == nil {
		t.Fatal("want error for unhealthy member")
	}
	if err := cl.Health(bg, "etcd-0:2379"); err != nil {
		t.Fatalf("other member: %v", err)
	}
	if _, err := cl.Status(bg, "etcd-1:2379"); err != nil {
		t.Fatalf("Status must not fail for an unhealthy member: %v", err)
	}
}

func TestUnknownEndpointNamesIt(t *testing.T) {
	cl := NewHealthy(1).Client()
	for name, err := range map[string]error{
		"Health":     cl.Health(bg, "nope:1"),
		"Defragment": cl.Defragment(bg, "nope:1"),
		"MoveLeader": cl.MoveLeader(bg, "nope:1", 1),
		"Status": func() error {
			_, err := cl.Status(bg, "nope:1")
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "nope:1") {
			t.Errorf("%s: err = %v, want one naming the endpoint", name, err)
		}
	}
}

func TestMoveLeaderUpdatesEveryStatus(t *testing.T) {
	c := NewHealthy(3)
	cl := c.Client()
	if err := cl.MoveLeader(bg, "etcd-0:2379", 3); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"etcd-0:2379", "etcd-1:2379", "etcd-2:2379"} {
		if l, _ := status(t, cl, ep); l != 3 {
			t.Errorf("%s leader = %d, want 3", ep, l)
		}
	}
	if c.Leader() != 3 {
		t.Fatalf("Leader() = %d", c.Leader())
	}
	if got := c.MoveLeaderLog(); !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("MoveLeaderLog = %v", got)
	}
}

func TestStalledMoveLeavesLeader(t *testing.T) {
	c := NewHealthy(3)
	c.StallLeaderMove(true)
	cl := c.Client()
	if err := cl.MoveLeader(bg, "etcd-0:2379", 2); err != nil {
		t.Fatal(err)
	}
	if c.Leader() != 1 {
		t.Fatalf("Leader() = %d, want unchanged 1", c.Leader())
	}
	if got := c.MoveLeaderLog(); !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("MoveLeaderLog = %v, want the stalled attempt recorded", got)
	}
	c.StallLeaderMove(false)
	if err := cl.MoveLeader(bg, "etcd-0:2379", 2); err != nil {
		t.Fatal(err)
	}
	if c.Leader() != 2 {
		t.Fatalf("Leader() = %d, want 2 once unstalled", c.Leader())
	}
}

func TestMoveLeaderToLearnerOrUnknownErrors(t *testing.T) {
	c := New(
		Member{ID: 1, Name: "a", Endpoint: "a", Healthy: true, DBSize: 2},
		Member{ID: 2, Name: "b", Endpoint: "b", Healthy: true, DBSize: 2, Learner: true},
	)
	cl := c.Client()
	if err := cl.MoveLeader(bg, "a", 2); err == nil {
		t.Error("learner target: want error")
	}
	if err := cl.MoveLeader(bg, "a", 99); err == nil {
		t.Error("unknown target: want error")
	}
	if c.Leader() != 1 || len(c.MoveLeaderLog()) != 0 {
		t.Fatalf("leader=%d log=%v, want unchanged and empty", c.Leader(), c.MoveLeaderLog())
	}
}

func TestStatusFields(t *testing.T) {
	c := NewHealthy(3)
	s, err := c.Client().Status(bg, "etcd-2:2379")
	if err != nil {
		t.Fatal(err)
	}
	if s.Header.MemberId != 3 || s.Header.ClusterId == 0 || s.Leader != 1 ||
		s.DbSize != DefaultDBSize || s.IsLearner || len(s.Errors) != 0 {
		t.Fatalf("unexpected status %+v", s)
	}
	s0, _ := c.Client().Status(bg, "etcd-0:2379")
	if s0.Header.ClusterId != s.Header.ClusterId {
		t.Fatal("cluster id differs between members")
	}
}

func TestAlarmShowsAsStatusError(t *testing.T) {
	c := NewHealthy(2)
	c.SetAlarm("etcd-1", "NOSPACE")
	cl := c.Client()
	s, _ := cl.Status(bg, "etcd-1:2379")
	if !reflect.DeepEqual(s.Errors, []string{"NOSPACE"}) {
		t.Fatalf("Errors = %v", s.Errors)
	}
	if s, _ := cl.Status(bg, "etcd-0:2379"); len(s.Errors) != 0 {
		t.Fatalf("alarm leaked to other member: %v", s.Errors)
	}
	c.SetAlarm("etcd-1")
	if s, _ := cl.Status(bg, "etcd-1:2379"); len(s.Errors) != 0 {
		t.Fatalf("alarm not cleared: %v", s.Errors)
	}
}

func TestSplitViewShowsThroughOnlyThatMember(t *testing.T) {
	c := NewHealthy(3)
	c.SplitLeaderView("etcd-2", 2)
	cl := c.Client()
	for ep, want := range map[string]uint64{"etcd-0:2379": 1, "etcd-1:2379": 1, "etcd-2:2379": 2} {
		if l, _ := status(t, cl, ep); l != want {
			t.Errorf("%s leader = %d, want %d", ep, l, want)
		}
	}
}

func TestMembersSortedAndRemoveMember(t *testing.T) {
	c := New(
		Member{ID: 9, Name: "z", Endpoint: "z:1", Healthy: true, DBSize: 2},
		Member{ID: 3, Name: "a", Endpoint: "a:1", Healthy: true, DBSize: 2, Learner: true},
		Member{ID: 5, Name: "m", Endpoint: "m:1", Healthy: true, DBSize: 2},
	)
	cl := c.Client()
	ids := func() []uint64 {
		r, err := cl.Members(bg)
		if err != nil {
			t.Fatal(err)
		}
		var out []uint64
		for _, m := range r.Members {
			out = append(out, m.ID)
		}
		return out
	}
	if got := ids(); !reflect.DeepEqual(got, []uint64{3, 5, 9}) {
		t.Fatalf("ids = %v", got)
	}
	r, _ := cl.Members(bg)
	if !r.Members[0].IsLearner || r.Members[1].Name != "m" ||
		!reflect.DeepEqual(r.Members[1].ClientURLs, []string{"m:1"}) {
		t.Fatalf("member fields wrong: %+v", r.Members)
	}
	c.RemoveMember("m")
	if got := ids(); !reflect.DeepEqual(got, []uint64{3, 9}) {
		t.Fatalf("ids after remove = %v", got)
	}
	if _, err := cl.Status(bg, "m:1"); err == nil {
		t.Fatal("removed member still answers Status")
	}
}

func TestRemoveLeaderElectsLowestVoter(t *testing.T) {
	c := New(
		Member{ID: 7, Name: "lead", Endpoint: "l", Healthy: true, DBSize: 2},
		Member{ID: 1, Name: "learn", Endpoint: "x", Healthy: true, DBSize: 2, Learner: true},
		Member{ID: 4, Name: "v4", Endpoint: "v4", Healthy: true, DBSize: 2},
		Member{ID: 3, Name: "v3", Endpoint: "v3", Healthy: true, DBSize: 2},
	)
	c.RemoveMember("lead")
	if c.Leader() != 3 {
		t.Fatalf("Leader() = %d, want 3", c.Leader())
	}
}

func TestFailNextFiresExactlyOncePerMethod(t *testing.T) {
	cl := func(c *Cluster) map[string]func() error {
		k := c.Client()
		return map[string]func() error{
			"Status": func() error { _, err := k.Status(bg, "etcd-0:2379"); return err },
			"Members": func() error {
				_, err := k.Members(bg)
				return err
			},
			"Health":     func() error { return k.Health(bg, "etcd-0:2379") },
			"MoveLeader": func() error { return k.MoveLeader(bg, "etcd-0:2379", 2) },
			"Defragment": func() error { return k.Defragment(bg, "etcd-0:2379") },
		}
	}
	for method := range cl(NewHealthy(1)) {
		t.Run(method, func(t *testing.T) {
			c := NewHealthy(3)
			calls := cl(c)
			boom := errors.New("boom " + method)
			c.FailNext(method, boom)
			for other, call := range calls {
				if other != method {
					if err := call(); err != nil {
						t.Fatalf("%s failed from FailNext(%s): %v", other, method, err)
					}
				}
			}
			if err := calls[method](); !errors.Is(err, boom) {
				t.Fatalf("first call err = %v, want boom", err)
			}
			if err := calls[method](); err != nil {
				t.Fatalf("second call err = %v, want nil", err)
			}
		})
	}
}

func TestFailNextDoesNotApplyTheEffect(t *testing.T) {
	c := NewHealthy(2)
	c.FailNext("Defragment", errors.New("boom"))
	_ = c.Client().Defragment(bg, "etcd-0:2379")
	if len(c.DefragLog()) != 0 || c.DBSize("etcd-0") != DefaultDBSize {
		t.Fatal("failed call had an effect")
	}
}

func TestFailNextQueues(t *testing.T) {
	c := NewHealthy(1)
	e1, e2 := errors.New("one"), errors.New("two")
	c.FailNext("Health", e1)
	c.FailNext("Health", e2)
	cl := c.Client()
	if err := cl.Health(bg, "etcd-0:2379"); !errors.Is(err, e1) {
		t.Fatalf("got %v", err)
	}
	if err := cl.Health(bg, "etcd-0:2379"); !errors.Is(err, e2) {
		t.Fatalf("got %v", err)
	}
	if err := cl.Health(bg, "etcd-0:2379"); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestHookRunsBeforeEffectAndSeesEndpoint(t *testing.T) {
	c := NewHealthy(3)
	var seen []string
	var sizeInHook int64
	c.Hook("Defragment", func(ep string) {
		seen = append(seen, ep)
		sizeInHook = c.DBSize("etcd-1")
	})
	if err := c.Client().Defragment(bg, "etcd-1:2379"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"etcd-1:2379"}) {
		t.Fatalf("seen = %v", seen)
	}
	if sizeInHook != DefaultDBSize {
		t.Fatalf("hook saw size %d, want the pre-effect size", sizeInHook)
	}
}

func TestHookCanInterleaveAFault(t *testing.T) {
	c := NewHealthy(3)
	c.Hook("Defragment", func(string) { c.SetHealthy("etcd-1", false) })
	if err := c.Client().Defragment(bg, "etcd-1:2379"); err == nil {
		t.Fatal("hook's fault must be visible to the call it precedes")
	}
	if len(c.DefragLog()) != 0 {
		t.Fatal("defrag logged despite mid-call fault")
	}
}

func TestHookOnMembersGetsEmptyEndpoint(t *testing.T) {
	c := NewHealthy(2)
	var got []string
	c.Hook("Members", func(ep string) { got = append(got, ep) })
	c.Hook("Status", func(string) { t.Error("Status hook ran for Members") })
	if _, err := c.Client().Members(bg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("got %v", got)
	}
}

func TestHookRemoveMemberMidStep(t *testing.T) {
	c := NewHealthy(3)
	c.Hook("Status", func(string) { c.RemoveMember("etcd-2") })
	cl := c.Client()
	if _, err := cl.Status(bg, "etcd-0:2379"); err != nil {
		t.Fatal(err)
	}
	r, _ := cl.Members(bg)
	if len(r.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(r.Members))
	}
}

func TestClosedClientErrors(t *testing.T) {
	c := NewHealthy(2)
	cl := c.Client()
	other := c.Client()
	cl.Close()
	cl.Close()
	for name, err := range allCalls(cl) {
		if err == nil {
			t.Errorf("%s on closed client: want error", name)
		}
	}
	for name, err := range allCalls(other) {
		if err != nil && name != "MoveLeader" {
			t.Errorf("%s on sibling client: %v", name, err)
		}
	}
	if len(c.DefragLog()) != 1 {
		t.Fatalf("closed client's Defragment had effect: %v", c.DefragLog())
	}
}

func TestCancelledContextErrors(t *testing.T) {
	c := NewHealthy(2)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	cl := c.Client()
	if _, err := cl.Status(ctx, "etcd-0:2379"); !errors.Is(err, context.Canceled) {
		t.Errorf("Status: %v", err)
	}
	if _, err := cl.Members(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Members: %v", err)
	}
	if err := cl.Health(ctx, "etcd-0:2379"); !errors.Is(err, context.Canceled) {
		t.Errorf("Health: %v", err)
	}
	if err := cl.MoveLeader(ctx, "etcd-0:2379", 2); !errors.Is(err, context.Canceled) {
		t.Errorf("MoveLeader: %v", err)
	}
	if err := cl.Defragment(ctx, "etcd-0:2379"); !errors.Is(err, context.Canceled) {
		t.Errorf("Defragment: %v", err)
	}
	if len(c.DefragLog()) != 0 || len(c.MoveLeaderLog()) != 0 || c.Leader() != 1 {
		t.Fatal("cancelled call had an effect")
	}
}

func TestCancelledContextSkipsHookAndQueuedFailure(t *testing.T) {
	c := NewHealthy(1)
	hooked := 0
	c.Hook("Health", func(string) { hooked++ })
	c.FailNext("Health", errors.New("boom"))
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_ = c.Client().Health(ctx, "etcd-0:2379")
	if hooked != 0 {
		t.Fatal("hook ran under a cancelled ctx")
	}
	if err := c.Client().Health(bg, "etcd-0:2379"); err == nil {
		t.Fatal("queued failure was consumed by the cancelled call")
	}
}

func allCalls(cl Client) map[string]error {
	out := map[string]error{}
	_, out["Status"] = cl.Status(bg, "etcd-0:2379")
	_, out["Members"] = cl.Members(bg)
	out["Health"] = cl.Health(bg, "etcd-0:2379")
	out["MoveLeader"] = cl.MoveLeader(bg, "etcd-0:2379", 1)
	out["Defragment"] = cl.Defragment(bg, "etcd-0:2379")
	return out
}

func TestConcurrentUse(t *testing.T) {
	c := NewHealthy(3)
	c.Hook("Status", func(string) { _ = c.Leader() })
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := c.Client()
			defer cl.Close()
			for range 50 {
				allCalls(cl)
				c.SetHealthy("etcd-1", i%2 == 0)
				c.SetAlarm("etcd-2", "A")
				c.SplitLeaderView("etcd-2", 2)
				c.StallLeaderMove(i%3 == 0)
				c.FailNext("Health", errors.New("x"))
				c.DefragLog()
				c.MoveLeaderLog()
				c.DBSize("etcd-0")
			}
		}()
	}
	wg.Wait()
}

func TestUnknownNamesPanic(t *testing.T) {
	c := NewHealthy(1)
	for name, fn := range map[string]func(){
		"SetHealthy": func() { c.SetHealthy("nope", true) },
		"FailNext":   func() { c.FailNext("Bogus", nil) },
		"Hook":       func() { c.Hook("Bogus", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: want panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestNewHealthyAtUsesTheGivenEndpoints(t *testing.T) {
	eps := []string{"ts-0.h.ns.svc:2379", "ts-1.h.ns.svc:2379", "ts-2.h.ns.svc:2379"}
	c := NewHealthyAt(eps...)
	cl := c.Client()
	for i, ep := range eps {
		s, err := cl.Status(bg, ep)
		if err != nil {
			t.Fatalf("Status(%s): %v", ep, err)
		}
		if s.Header.MemberId != uint64(i+1) || s.Leader != 1 {
			t.Fatalf("Status(%s) member=%d leader=%d", ep, s.Header.MemberId, s.Leader)
		}
	}
	if _, err := cl.Status(bg, "etcd-0:2379"); err == nil {
		t.Fatal("the default endpoint must not exist when endpoints were given")
	}
}

func TestNewHealthyDefaultsAreReclaimable(t *testing.T) {
	s, err := NewHealthy(1).Client().Status(bg, "etcd-0:2379")
	if err != nil {
		t.Fatal(err)
	}
	// The operator's minimumReclaimBytes is 100 MiB and its minimum free ratio
	// is 0.3; the defaults must clear both or no test ever defragments.
	free := s.DbSize - s.DbSizeInUse
	if free < 100<<20 || float64(free)/float64(s.DbSize) < 0.3 {
		t.Fatalf(
			"size=%d inUse=%d: not reclaimable by the operator's rule",
			s.DbSize,
			s.DbSizeInUse,
		)
	}
}

func TestSetDBSizeSetsSizeAndInUse(t *testing.T) {
	c := NewHealthy(2)
	c.SetDBSize("etcd-1", 1000, 10)
	s, err := c.Client().Status(bg, "etcd-1:2379")
	if err != nil {
		t.Fatal(err)
	}
	if s.DbSize != 1000 || s.DbSizeInUse != 10 {
		t.Fatalf("size=%d inUse=%d, want 1000 and 10", s.DbSize, s.DbSizeInUse)
	}
}

func TestMoveLeaderRequiresTheLeaderEndpoint(t *testing.T) {
	c := NewHealthy(3)
	cl := c.Client()
	if err := cl.MoveLeader(bg, "etcd-1:2379", 3); err == nil {
		t.Fatal("MoveLeader sent to a follower: want error")
	}
	if c.Leader() != 1 || len(c.MoveLeaderLog()) != 0 {
		t.Fatalf("refused move had an effect: leader=%d log=%v", c.Leader(), c.MoveLeaderLog())
	}
	if err := cl.MoveLeader(bg, "etcd-0:2379", 3); err != nil {
		t.Fatalf("MoveLeader to the leader's endpoint: %v", err)
	}
	if err := cl.MoveLeader(bg, "etcd-0:2379", 2); err == nil {
		t.Fatal("the old leader's endpoint must be refused after the move")
	}
}

func TestSetDBSizePanicsOnInvalidSizes(t *testing.T) {
	tests := map[string][2]int64{
		"in use above size": {10, 20},
		"negative in use":   {10, -1},
		"zero size":         {0, 0},
		"negative size":     {-5, -5},
	}
	for name, sizes := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewHealthy(1)
			before := c.DBSize("etcd-0")
			defer func() {
				if recover() == nil {
					t.Fatal("SetDBSize did not panic")
				}
				if got := c.DBSize("etcd-0"); got != before {
					t.Fatalf("a refused SetDBSize changed the size: %d, want %d", got, before)
				}
			}()
			c.SetDBSize("etcd-0", sizes[0], sizes[1])
		})
	}
}

func TestNewPanicsOnInvalidMembers(t *testing.T) {
	ok := func(id uint64, name, ep string) Member {
		return Member{ID: id, Name: name, Endpoint: ep, Healthy: true}
	}
	tests := map[string][]Member{
		"duplicate name":     {ok(1, "a", "x"), ok(2, "a", "y")},
		"duplicate endpoint": {ok(1, "a", "x"), ok(2, "b", "x")},
		"duplicate id":       {ok(1, "a", "x"), ok(1, "b", "y")},
		"learner leader": {
			{ID: 1, Name: "a", Endpoint: "x", Healthy: true, Learner: true},
			ok(2, "b", "y"),
		},
	}
	for name, members := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			New(members...)
		})
	}
}

func TestSecondDefragmentDoesNotGoBelowInUse(t *testing.T) {
	c := NewHealthy(1)
	cl := c.Client()
	// Three passes: halving would land exactly on DefaultInUse after two.
	for range 3 {
		if err := cl.Defragment(bg, "etcd-0:2379"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := cl.Status(bg, "etcd-0:2379")
	if err != nil {
		t.Fatal(err)
	}
	if s.DbSize != DefaultInUse || s.DbSizeInUse != DefaultInUse {
		t.Fatalf(
			"size=%d inUse=%d after three defrags, want both %d",
			s.DbSize,
			s.DbSizeInUse,
			DefaultInUse,
		)
	}
}

// SPDX-License-Identifier: Apache-2.0

package ctrltest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ownedPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p",
		ManagedFields: []metav1.ManagedFieldsEntry{
			{
				Manager:    "alpha",
				Operation:  metav1.ManagedFieldsOperationApply,
				FieldsType: "FieldsV1",
				FieldsV1:   metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:a":{}}}}`),
			},
			{
				Manager:     "beta",
				Subresource: "status",
				Operation:   metav1.ManagedFieldsOperationUpdate,
				FieldsType:  "FieldsV1",
				FieldsV1:    metav1.NewFieldsV1(`{"f:status":{"f:phase":{}}}`),
			},
		},
	}}
}

func TestC_OwnershipAssertionsPass(t *testing.T) {
	c := Bare(t)
	p := ownedPod()
	c.OwnedBy(p, "alpha", ".metadata.labels.a")
	c.OwnedBy(p, "beta", ".status.phase")
	c.NotOwned(p, ".metadata.labels.b")
	c.NotOwned(p, ".metadata.labels")
	c.NoManager(p, "gamma", "delta")
	c.NoManager(p)
}

func TestC_OwnershipAssertionsFail(t *testing.T) {
	p := ownedPod()
	for name, tc := range map[string]struct {
		call func(*C)
		want []string
	}{
		"OwnedBy wrong manager": {
			func(c *C) { c.OwnedBy(p, "beta", ".metadata.labels.a") },
			[]string{"ns/p", `"beta"`, ".metadata.labels.a", `["alpha"]`},
		},
		"OwnedBy unclaimed path": {
			func(c *C) { c.OwnedBy(p, "alpha", ".metadata.labels.zzz") },
			[]string{"ns/p", ".metadata.labels.zzz"},
		},
		"OwnedBy prefix is not ownership": {
			func(c *C) { c.OwnedBy(p, "alpha", ".metadata.labels") },
			[]string{".metadata.labels"},
		},
		"NotOwned": {
			func(c *C) { c.NotOwned(p, ".status.phase") },
			[]string{"ns/p", ".status.phase", `["beta"]`},
		},
		"NoManager": {
			func(c *C) { c.NoManager(p, "gamma", "beta", "alpha") },
			[]string{"ns/p", `"alpha", "beta"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			tc.call(Bare(r))
			if len(r.fatals) != 1 {
				t.Fatalf("fatals = %q, want exactly one", r.fatals)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.fatals[0], w) {
					t.Errorf("message %q lacks %q", r.fatals[0], w)
				}
			}
		})
	}
}

func TestC_OwnershipRefusesUndecodableManagedFields(t *testing.T) {
	p := ownedPod()
	p.ManagedFields[0].FieldsV1.SetRawString(`{not json`)
	r := &recorder{}
	func() {
		defer func() { _ = recover() }()
		Bare(r).NotOwned(p, ".metadata.labels.a")
	}()
	if len(r.fatals) == 0 {
		t.Fatal("NotOwned passed on managedFields it could not decode")
	}
}

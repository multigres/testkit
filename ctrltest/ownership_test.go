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
		t.Run(name+"/Require", func(t *testing.T) {
			r := &recorder{}
			tc.call(Bare(r).Require())
			if len(r.fatals) != 1 || len(r.errors) != 0 {
				t.Fatalf(
					"fatals = %q, errors = %q, want one fatal and no errors",
					r.fatals,
					r.errors,
				)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.fatals[0], w) {
					t.Errorf("message %q lacks %q", r.fatals[0], w)
				}
			}
		})
		t.Run(name+"/Check", func(t *testing.T) {
			r := &recorder{}
			tc.call(Bare(r).Check())
			if len(r.errors) != 1 || len(r.fatals) != 0 {
				t.Fatalf(
					"fatals = %q, errors = %q, want one error and no fatals",
					r.fatals,
					r.errors,
				)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.errors[0], w) {
					t.Errorf("message %q lacks %q", r.errors[0], w)
				}
			}
		})
	}
}

// The recorder's Fatalf returns instead of exiting the goroutine, as it does
// in every collecting-mode case, so reaching Owners after a failed decode
// would panic here. Each call must stop at the decode failure on its own.
func TestC_OwnershipRefusesUndecodableManagedFields(t *testing.T) {
	calls := map[string]func(*C, *corev1.Pod){
		"OwnedBy":   func(c *C, p *corev1.Pod) { c.OwnedBy(p, "alpha", ".metadata.labels.a") },
		"NotOwned":  func(c *C, p *corev1.Pod) { c.NotOwned(p, ".metadata.labels.a") },
		"NoManager": func(c *C, p *corev1.Pod) { c.NoManager(p, "gamma") },
	}
	for name, call := range calls {
		for mode, view := range map[string]func(*C) *C{
			"Require": (*C).Require,
			"Check":   (*C).Check,
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				p := ownedPod()
				p.ManagedFields[0].FieldsV1.SetRawString(`{not json`)
				r := &recorder{}
				call(view(Bare(r)), p)
				if len(r.fatals)+len(r.errors) != 1 {
					t.Fatalf(
						"fatals = %q, errors = %q, want exactly one failure",
						r.fatals,
						r.errors,
					)
				}
			})
		}
	}
}

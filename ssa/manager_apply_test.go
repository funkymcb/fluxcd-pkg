/*
Copyright 2021 Stefan Prodan
Copyright 2021 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ssa

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluxcd/pkg/ssa/normalize"
	"github.com/fluxcd/pkg/ssa/utils"
)

func TestApply(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("apply")
	objects, err := readManifest("testdata/test1.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "app1", "default")

	configMapName, configMap := getFirstObject(objects, "ConfigMap", id)

	t.Run("creates objects in order", func(t *testing.T) {
		// create objects
		changeSet, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		// expected created order
		sort.Sort(SortableUnstructureds(objects))
		var expected []string
		for _, object := range objects {
			expected = append(expected, utils.FmtUnstructured(object))
		}

		// verify the change set contains only created actions
		var output []string
		for _, entry := range changeSet.Entries {
			if diff := cmp.Diff(entry.Action, CreatedAction); diff != "" {
				t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
			}
			output = append(output, entry.Subject)
		}

		// verify the change set contains all objects in the right order
		if diff := cmp.Diff(expected, output); diff != "" {
			t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
		}
	})

	t.Run("does not apply unchanged objects", func(t *testing.T) {
		// no-op apply
		changeSet, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		// verify the change set contains only unchanged actions
		for _, entry := range changeSet.Entries {
			if diff := cmp.Diff(UnchangedAction, entry.Action); diff != "" {
				t.Errorf("Mismatch from expected value (-want +got):\n%s\n%v", diff, changeSet)
			}
		}
	})

	t.Run("applies only changed objects", func(t *testing.T) {
		// update a value in the configmap
		err = unstructured.SetNestedField(configMap.Object, "val", "data", "key")
		if err != nil {
			t.Fatal(err)
		}

		// apply changes
		changeSet, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		// verify the change set contains the configured action only for the configmap
		for _, entry := range changeSet.Entries {
			if entry.Subject == configMapName {
				if diff := cmp.Diff(ConfiguredAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(UnchangedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			}
		}

		// get the configmap from cluster
		configMapClone := configMap.DeepCopy()
		err = manager.client.Get(ctx, client.ObjectKeyFromObject(configMapClone), configMapClone)
		if err != nil {
			t.Fatal(err)
		}

		// get data value from the in-cluster configmap
		val, _, err := unstructured.NestedFieldCopy(configMapClone.Object, "data", "key")
		if err != nil {
			t.Fatal(err)
		}

		// verify the configmap was updated in cluster with the right data value
		if diff := cmp.Diff(val, "val"); diff != "" {
			t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
		}
	})
}

func TestApply_Force(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("apply")
	objects, err := readManifest("testdata/test1.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "app1", "default")

	secretName, secret := getFirstObject(objects, "Secret", id)
	crbName, crb := getFirstObject(objects, "ClusterRoleBinding", id)
	stName, st := getFirstObject(objects, "StorageClass", id)
	_, svc := getFirstObject(objects, "Service", id)

	// create objects
	if _, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions()); err != nil {
		t.Fatal(err)
	}

	t.Run("fails to apply immutable secret", func(t *testing.T) {
		// update a value in the secret
		err = unstructured.SetNestedField(secret.Object, "val-secret", "stringData", "key")
		if err != nil {
			t.Fatal(err)
		}

		// apply and expect to fail
		_, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err == nil {
			t.Fatal("Expected error got none")
		}

		// verify that the error message does not contain sensitive information
		expectedErr := fmt.Sprintf(
			"%s dry-run failed (Invalid): Secret \"%s\" is invalid: data: Forbidden: field is immutable when `immutable` is set",
			utils.FmtUnstructured(secret), secret.GetName())
		if diff := cmp.Diff(expectedErr, err.Error()); diff != "" {
			t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
		}
	})

	t.Run("force applies immutable secret", func(t *testing.T) {
		// force apply
		opts := DefaultApplyOptions()
		opts.Force = true

		changeSet, err := manager.ApplyAllStaged(ctx, objects, opts)
		if err != nil {
			t.Fatal(err)
		}

		// verify the secret was recreated
		for _, entry := range changeSet.Entries {
			if entry.Subject == secretName {
				if diff := cmp.Diff(CreatedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(UnchangedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			}
		}

		// get the secret from cluster
		secretClone := secret.DeepCopy()
		err = manager.client.Get(ctx, client.ObjectKeyFromObject(secretClone), secretClone)
		if err != nil {
			t.Fatal(err)
		}

		// get data value from the in-cluster secret
		val, _, err := unstructured.NestedFieldCopy(secretClone.Object, "data", "key")
		if err != nil {
			t.Fatal(err)
		}

		// verify the secret was updated in cluster with the right data value
		if diff := cmp.Diff(val, base64.StdEncoding.EncodeToString([]byte("val-secret"))); diff != "" {
			t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
		}
	})

	t.Run("force apply waits for finalizer", func(t *testing.T) {
		secretClone := secret.DeepCopy()
		{
			secretWithFinalizer := secretClone.DeepCopy()

			unstructured.SetNestedStringSlice(secretWithFinalizer.Object, []string{"fluxcd.io/demo-finalizer"}, "metadata", "finalizers")
			if err := manager.client.Update(ctx, secretWithFinalizer); err != nil {
				t.Fatal(err)
			}
		}

		// remove finalizer after a delay, to ensure the controller handles a slow deletion
		go func() {
			time.Sleep(3 * time.Second)

			secretWithoutFinalizer := secretClone.DeepCopy()
			unstructured.SetNestedStringSlice(secretWithoutFinalizer.Object, []string{}, "metadata", "finalizers")
			if err := manager.client.Update(ctx, secretWithoutFinalizer); err != nil {
				panic(err)
			}
		}()

		// update a value in the secret
		err = unstructured.SetNestedField(secret.Object, "val-secret2", "stringData", "key")
		if err != nil {
			t.Fatal(err)
		}

		// force apply
		opts := DefaultApplyOptions()
		opts.Force = true

		changeSet, err := manager.ApplyAllStaged(ctx, objects, opts)
		if err != nil {
			t.Fatal(err)
		}

		// verify the secret was recreated
		for _, entry := range changeSet.Entries {
			if entry.Subject == secretName {
				if diff := cmp.Diff(CreatedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(UnchangedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
			}
		}
	})

	t.Run("recreates immutable RBAC", func(t *testing.T) {
		// update roleRef
		err = unstructured.SetNestedField(crb.Object, "test", "roleRef", "name")
		if err != nil {
			t.Fatal(err)
		}

		// force apply
		opts := DefaultApplyOptions()
		opts.Force = true

		changeSet, err := manager.ApplyAllStaged(ctx, objects, opts)
		if err != nil {
			t.Fatal(err)
		}

		// verify the binding was recreated
		for _, entry := range changeSet.Entries {
			if entry.Subject == crbName {
				if diff := cmp.Diff(CreatedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
				break
			}
		}
	})

	t.Run("recreates immutable StorageClass based on metadata", func(t *testing.T) {
		// update parameters
		err = unstructured.SetNestedField(st.Object, "true", "parameters", "encrypted")
		if err != nil {
			t.Fatal(err)
		}

		meta := map[string]string{
			"fluxcd.io/force": "true",
		}
		st.SetAnnotations(meta)

		// apply and expect to fail
		_, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err == nil {
			t.Fatal("Expected error got none")
		}

		// force apply selector
		opts := DefaultApplyOptions()
		opts.ForceSelector = meta

		changeSet, err := manager.ApplyAllStaged(ctx, objects, opts)
		if err != nil {
			t.Fatal(err)
		}

		// verify the storage class was recreated
		for _, entry := range changeSet.Entries {
			if entry.Subject == stName {
				if diff := cmp.Diff(CreatedAction, entry.Action); diff != "" {
					t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
				}
				break
			}
		}
	})

	t.Run("force apply returns validation error", func(t *testing.T) {
		// update to invalid yaml
		err = unstructured.SetNestedField(svc.Object, "ClusterIPSS", "spec", "type")
		if err != nil {
			t.Fatal(err)
		}

		// force apply objects
		opts := DefaultApplyOptions()
		opts.Force = true

		_, err := manager.ApplyAllStaged(ctx, objects, opts)
		if err == nil {
			t.Fatal("expected validation error but got none")
		}

		// should return validation error
		if !strings.Contains(err.Error(), "is invalid") {
			t.Errorf("expected error to contain invalid msg but got: %s", err)
		}
	})
}

func TestApply_SetNativeKindsDefaults(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("fix")
	objects, err := readManifest("testdata/test2.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "app1", "default")

	if err := normalize.UnstructuredList(objects); err != nil {
		t.Fatal(err)
	}

	t.Run("creates objects", func(t *testing.T) {
		// create objects
		_, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
	})

	// re-apply objects
	changeSet, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
	if err != nil {
		t.Fatal(err)
	}

	// verify that the change set contains no changed objects
	for _, entry := range changeSet.Entries {
		if diff := cmp.Diff(UnchangedAction, entry.Action); diff != "" {
			t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
		}
	}

}

func TestApply_NoOp(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("fix")
	objects, err := readManifest("testdata/test3.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "app1", "default")

	if err := normalize.UnstructuredList(objects); err != nil {
		t.Fatal(err)
	}

	t.Run("creates objects", func(t *testing.T) {
		// create objects
		_, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("skips apply", func(t *testing.T) {
		// apply changes
		changeSet, err := manager.ApplyAll(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if entry.Action != UnchangedAction {
				t.Errorf("Diff found for %s", entry.String())
			}
		}
	})
}

func TestApply_Exclusions(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("ignore")
	objects, err := readManifest("testdata/test1.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	_, configMap := getFirstObject(objects, "ConfigMap", id)

	t.Run("creates objects", func(t *testing.T) {
		// create objects
		_, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("skips apply", func(t *testing.T) {
		// mutate in-cluster object
		configMapClone := configMap.DeepCopy()
		err = manager.client.Get(ctx, client.ObjectKeyFromObject(configMapClone), configMapClone)
		if err != nil {
			t.Fatal(err)
		}

		meta := map[string]string{
			"fluxcd.io/ignore": "true",
		}
		configMapClone.SetAnnotations(meta)

		if err := unstructured.SetNestedField(configMapClone.Object, "val", "data", "key"); err != nil {
			t.Fatal(err)
		}

		if err := manager.client.Update(ctx, configMapClone); err != nil {
			t.Fatal(err)
		}

		opts := DefaultApplyOptions()
		opts.ExclusionSelector = meta

		// apply with exclusions
		changeSet, err := manager.ApplyAll(ctx, objects, opts)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if entry.Action == ConfiguredAction {
				t.Errorf("Diff found for %s", entry.String())
			}
		}
	})

	t.Run("applies changes", func(t *testing.T) {
		// apply changes without exclusions
		changeSet, err := manager.ApplyAll(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if entry.Action != ConfiguredAction && entry.Subject == utils.FmtUnstructured(configMap) {
				t.Errorf("Expected %s, got %s", ConfiguredAction, entry.Action)
			}
		}
	})

	t.Run("skips apply when desired state is annotated", func(t *testing.T) {
		configMapClone := configMap.DeepCopy()
		meta := map[string]string{
			"fluxcd.io/ignore": "true",
		}
		configMapClone.SetAnnotations(meta)

		// apply changes without exclusions
		changeSet, err := manager.Apply(ctx, configMapClone, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		if changeSet.Action != UnchangedAction {
			t.Errorf("Diff found for %s", changeSet.String())
		}
	})
}

func TestApply_IfNotPresent(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	meta := map[string]string{
		"fluxcd.io/ssa": "IfNotPresent",
	}

	id := generateName("skip")
	objects, err := readManifest("testdata/test1.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	_, ns := getFirstObject(objects, "Namespace", id)
	_, configMap := getFirstObject(objects, "ConfigMap", id)
	configMapClone := configMap.DeepCopy()
	configMapClone.SetAnnotations(meta)

	t.Run("creates objects", func(t *testing.T) {
		// create objects
		opts := DefaultApplyOptions()
		opts.IfNotPresentSelector = meta

		changeSet, err := manager.ApplyAllStaged(ctx, []*unstructured.Unstructured{ns, configMapClone}, opts)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if entry.Action != CreatedAction {
				t.Errorf("Expected %s, got %s for %s", CreatedAction, entry.Action, entry.Subject)
			}
		}
	})

	t.Run("skips apply when annotated IfNotPresent", func(t *testing.T) {
		opts := DefaultApplyOptions()
		opts.IfNotPresentSelector = meta

		changeSet, err := manager.Apply(ctx, configMapClone, opts)
		if err != nil {
			t.Fatal(err)
		}

		if changeSet.Action != SkippedAction {
			t.Errorf("Diff found for %s", changeSet.String())
		}
	})

	t.Run("resume apply when is annotated Override", func(t *testing.T) {
		override := map[string]string{
			"fluxcd.io/ssa": "Override",
		}
		configMapClone.SetAnnotations(override)

		opts := DefaultApplyOptions()
		opts.IfNotPresentSelector = meta

		changeSet, err := manager.Apply(ctx, configMapClone, opts)
		if err != nil {
			t.Fatal(err)
		}

		if changeSet.Action != ConfiguredAction {
			t.Errorf("Diff found for %s", changeSet.String())
		}
	})
}

func TestApply_Cleanup(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	applyOpts := DefaultApplyOptions()
	applyOpts.Cleanup = ApplyCleanupOptions{
		Annotations: []string{corev1.LastAppliedConfigAnnotation},
		FieldManagers: []FieldManager{
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationApply,
			},
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				Name:          "before-first-apply",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
		},
	}

	id := generateName("cleanup")
	objects, err := readManifest("testdata/test2.yaml", id)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetOwnerLabels(objects, "app1", "default")

	_, deployObject := getFirstObject(objects, "Deployment", id)

	if err = normalize.UnstructuredList(objects); err != nil {
		t.Fatal(err)
	}

	t.Run("creates objects as kubectl", func(t *testing.T) {
		for _, object := range objects {
			obj := object.DeepCopy()
			obj.SetAnnotations(map[string]string{corev1.LastAppliedConfigAnnotation: "test"})
			labels := obj.GetLabels()
			labels[corev1.LastAppliedConfigAnnotation] = "test"
			obj.SetLabels(labels)
			if err := manager.client.Create(ctx, obj, client.FieldOwner("kubectl-client-side-apply")); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("removes kubectl client-side-apply manager and annotation", func(t *testing.T) {
		applyOpts.Cleanup.Labels = []string{corev1.LastAppliedConfigAnnotation}
		changeSet, err := manager.ApplyAllStaged(ctx, objects, applyOpts)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if diff := cmp.Diff(ConfiguredAction, entry.Action); diff != "" {
				t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
			}
		}

		deploy := deployObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(deploy), deploy)
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := deploy.GetAnnotations()[corev1.LastAppliedConfigAnnotation]; ok {
			t.Errorf("%s annotation not removed", corev1.LastAppliedConfigAnnotation)
		}

		if _, ok := deploy.GetLabels()[corev1.LastAppliedConfigAnnotation]; ok {
			t.Errorf("%s label not removed", corev1.LastAppliedConfigAnnotation)
		}

		expectedManagers := []string{"before-first-apply", manager.owner.Field}
		for _, entry := range deploy.GetManagedFields() {
			if !containsItemString(expectedManagers, entry.Manager) {
				t.Log(entry)
				t.Errorf("Mismatch from expected values, want %v got %s", expectedManagers, entry.Manager)
			}
		}
	})

	t.Run("replaces kubectl server-side-apply manager", func(t *testing.T) {
		for _, object := range objects {
			obj := object.DeepCopy()
			if err := manager.client.Patch(ctx, obj, client.Apply, client.FieldOwner("kubectl")); err != nil {
				t.Fatal(err)
			}
		}

		deploy := deployObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(deploy), deploy)
		if err != nil {
			t.Fatal(err)
		}

		changeSet, err := manager.ApplyAll(ctx, objects, applyOpts)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if diff := cmp.Diff(ConfiguredAction, entry.Action); diff != "" {
				t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
			}
		}

		deploy = deployObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(deploy), deploy)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range deploy.GetManagedFields() {
			if diff := cmp.Diff(manager.owner.Field, entry.Manager); diff != "" {
				t.Log(entry)
				t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
			}
		}
	})
}

func TestApply_CleanupRemovals(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	applyOpts := DefaultApplyOptions()
	applyOpts.Cleanup = ApplyCleanupOptions{
		Annotations: []string{corev1.LastAppliedConfigAnnotation},
		FieldManagers: []FieldManager{
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationApply,
			},
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				Name:          "before-first-apply",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
		},
	}

	id := generateName("cleanup-removal")
	objects, err := readManifest("testdata/test8.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	editedObjects, err := readManifest("testdata/test9.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "app1", "default")
	manager.SetOwnerLabels(editedObjects, "app1", "default")

	_, ingressObject := getFirstObject(objects, "Ingress", id)

	t.Run("creates objects using manager apply", func(t *testing.T) {
		applyOpts.Cleanup.Labels = []string{corev1.LastAppliedConfigAnnotation}
		_, err := manager.ApplyAllStaged(ctx, objects, applyOpts)
		if err != nil {
			t.Fatal(err)
		}

		ingress := ingressObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(ingress), ingress)
		if err != nil {
			t.Fatal(err)
		}

		expectedManagers := []string{manager.owner.Field}
		for _, entry := range ingress.GetManagedFields() {
			if !containsItemString(expectedManagers, entry.Manager) {
				t.Log(entry)
				t.Errorf("Mismatch from expected values, want %v got %s", expectedManagers, entry.Manager)
			}
		}
	})

	t.Run("applies edited objects as kubectl", func(t *testing.T) {
		for _, object := range editedObjects {
			obj := object.DeepCopy()
			obj.SetAnnotations(map[string]string{corev1.LastAppliedConfigAnnotation: "test"})
			labels := obj.GetLabels()
			labels[corev1.LastAppliedConfigAnnotation] = "test"
			obj.SetLabels(labels)
			if err := manager.client.Patch(ctx, obj, client.Merge, client.FieldOwner("kubectl-client-side-apply")); err != nil {
				t.Fatal(err)
			}
		}

		ingress := ingressObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(ingress), ingress)
		if err != nil {
			t.Fatal(err)
		}

		expectedManagers := []string{"kubectl-client-side-apply", manager.owner.Field}
		for _, entry := range ingress.GetManagedFields() {
			if !containsItemString(expectedManagers, entry.Manager) {
				t.Log(entry)
				t.Errorf("Mismatch from expected values, want %v got %s", expectedManagers, entry.Manager)
			}
		}

		rules, _, err := unstructured.NestedSlice(ingress.Object, "spec", "rules")
		if err != nil {
			t.Fatal(err)
		}

		if len(rules) != 2 {
			t.Errorf("expected to two rules in Ingress, got %d", len(rules))
		}
	})

	t.Run("applies edited object using manager apply", func(t *testing.T) {
		applyOpts.Cleanup.Labels = []string{corev1.LastAppliedConfigAnnotation}
		_, err := manager.ApplyAllStaged(ctx, editedObjects, applyOpts)
		if err != nil {
			t.Fatal(err)
		}

		ingress := ingressObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(ingress), ingress)
		if err != nil {
			t.Fatal(err)
		}

		expectedManagers := []string{manager.owner.Field}
		for _, entry := range ingress.GetManagedFields() {
			if !containsItemString(expectedManagers, entry.Manager) {
				t.Log(entry)
				t.Errorf("Mismatch from expected values, want %v got %s", expectedManagers, entry.Manager)
			}
		}

		tlsMap, exists, err := unstructured.NestedSlice(ingress.Object, "spec", "tls")
		if err != nil {
			t.Fatalf("unexpected error while getting field from object: %s", err)
		}

		if exists {
			t.Errorf("spec.tls shouldn't be present, got %s", tlsMap)
		}
	})
}

func TestApply_Cleanup_Exclusions(t *testing.T) {
	kubectlManager := "kubectl-client-side-apply"
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	applyOpts := DefaultApplyOptions()
	applyOpts.Cleanup = ApplyCleanupOptions{
		Annotations: []string{corev1.LastAppliedConfigAnnotation},
		FieldManagers: []FieldManager{
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationApply,
			},
			{
				Name:          "kubectl",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
			{
				Name:          "before-first-apply",
				OperationType: metav1.ManagedFieldsOperationUpdate,
			},
		},
		Exclusions: map[string]string{"cleanup/exclusion": "true"},
	}

	id := generateName("cleanup")
	objects, err := readManifest("testdata/test2.yaml", id)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetOwnerLabels(objects, "app1", "default")

	_, deployObject := getFirstObject(objects, "Deployment", id)

	if err = normalize.UnstructuredList(objects); err != nil {
		t.Fatal(err)
	}

	t.Run("creates objects as kubectl", func(t *testing.T) {
		for _, object := range objects {
			obj := object.DeepCopy()
			if err := manager.client.Create(ctx, obj, client.FieldOwner(kubectlManager)); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("does not not remove kubectl manager", func(t *testing.T) {
		for _, object := range objects {
			object.SetAnnotations(map[string]string{"cleanup/exclusion": "true"})
		}

		changeSet, err := manager.ApplyAllStaged(ctx, objects, applyOpts)
		if err != nil {
			t.Fatal(err)
		}

		for _, entry := range changeSet.Entries {
			if diff := cmp.Diff(ConfiguredAction, entry.Action); diff != "" {
				t.Errorf("Mismatch from expected value (-want +got):\n%s", diff)
			}
		}

		deploy := deployObject.DeepCopy()
		err = manager.Client().Get(ctx, client.ObjectKeyFromObject(deploy), deploy)
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, entry := range deploy.GetManagedFields() {
			if entry.Manager == kubectlManager {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("Mismatch from expected values, want %v manager", kubectlManager)
		}
	})
}

func containsItemString(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

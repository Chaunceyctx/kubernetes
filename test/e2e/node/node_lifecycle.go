/*
Copyright 2024 The Kubernetes Authors.

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

package node

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	e2essh "k8s.io/kubernetes/test/e2e/framework/ssh"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = SIGDescribe("Node Lifecycle", func() {

	f := framework.NewDefaultFramework("fake-node")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	/*
		Release: v1.32
		Testname: Node, resource lifecycle
		Description: Creating and Reading a Node MUST succeed with required name retrieved.
		Patching a Node MUST succeed with its new label found. Listing Nodes with a labelSelector
		MUST succeed with only a single node found. Updating a Node MUST succeed with
		its new label found. Deleting the Node MUST succeed and its deletion MUST be confirmed.
	*/
	framework.ConformanceIt("should run through the lifecycle of a node", func(ctx context.Context) {
		// the scope of this test only covers the api-server

		nodeClient := f.ClientSet.CoreV1().Nodes()

		// Create a fake node with a ready condition but unschedulable, so it won't be selected by
		// the scheduler and won't be deleted by the cloud controller manager when the test runs on
		// a specific cloud provider.
		fakeNode := v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "e2e-fake-node-" + utilrand.String(5),
			},
			Spec: v1.NodeSpec{
				Unschedulable: true,
			},
			Status: v1.NodeStatus{
				Phase: v1.NodeRunning,
				Conditions: []v1.NodeCondition{
					{
						Status:  v1.ConditionTrue,
						Message: "Set from e2e test",
						Reason:  "E2E",
						Type:    v1.NodeReady,
					},
				},
			},
		}

		ginkgo.By(fmt.Sprintf("Create %q", fakeNode.Name))
		createdNode, err := nodeClient.Create(ctx, &fakeNode, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create node %q", fakeNode.Name)
		gomega.Expect(createdNode.Name).To(gomega.Equal(fakeNode.Name), "Checking that the node has been created")

		ginkgo.By(fmt.Sprintf("Getting %q", fakeNode.Name))
		retrievedNode, err := nodeClient.Get(ctx, fakeNode.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to retrieve Node %q", fakeNode.Name)
		gomega.Expect(retrievedNode.Name).To(gomega.Equal(fakeNode.Name), "Checking that the retrieved name has been found")

		ginkgo.By(fmt.Sprintf("Patching %q", fakeNode.Name))
		payload := "{\"metadata\":{\"labels\":{\"" + fakeNode.Name + "\":\"patched\"}}}"
		patchedNode, err := nodeClient.Patch(ctx, fakeNode.Name, types.StrategicMergePatchType, []byte(payload), metav1.PatchOptions{})
		framework.ExpectNoError(err, "Failed to patch %q", fakeNode.Name)
		gomega.Expect(patchedNode.Labels).To(gomega.HaveKeyWithValue(fakeNode.Name, "patched"), "Checking that patched label has been applied")
		patchedSelector := labels.Set{fakeNode.Name: "patched"}.AsSelector().String()

		ginkgo.By(fmt.Sprintf("Listing nodes with LabelSelector %q", patchedSelector))
		nodes, err := nodeClient.List(ctx, metav1.ListOptions{LabelSelector: patchedSelector})
		framework.ExpectNoError(err, "failed to list nodes")
		gomega.Expect(nodes.Items).To(gomega.HaveLen(1), "confirm that the patched node has been found")

		ginkgo.By(fmt.Sprintf("Updating %q", fakeNode.Name))
		var updatedNode *v1.Node

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			tmpNode, err := nodeClient.Get(ctx, fakeNode.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "Unable to get %q", fakeNode.Name)
			tmpNode.Labels[fakeNode.Name] = "updated"
			updatedNode, err = nodeClient.Update(ctx, tmpNode, metav1.UpdateOptions{})

			return err
		})
		framework.ExpectNoError(err, "failed to update %q", fakeNode.Name)
		gomega.Expect(updatedNode.Labels).To(gomega.HaveKeyWithValue(fakeNode.Name, "updated"), "Checking that updated label has been applied")

		ginkgo.By(fmt.Sprintf("Delete %q", fakeNode.Name))
		err = nodeClient.Delete(ctx, fakeNode.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "failed to delete node")

		ginkgo.By(fmt.Sprintf("Confirm deletion of %q", fakeNode.Name))
		gomega.Eventually(ctx, func(ctx context.Context) error {
			_, err := nodeClient.Get(ctx, fakeNode.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("nodeClient.Get returned an unexpected error: %w", err)
			}
			return fmt.Errorf("node still exists: %s", fakeNode.Name)
		}, 3*time.Minute, 5*time.Second).Should(gomega.Succeed(), "Timeout while waiting to confirm Node deletion")
	})
	framework.ConformanceIt("should not wipe taint on node when node is restarting", func(ctx context.Context) {
		nodeClient := f.ClientSet.CoreV1().Nodes()

		nodes, err := nodeClient.List(ctx, metav1.ListOptions{})
		if err != nil || len(nodes.Items) == 0 {
			framework.Logf("no nodes exist in cluster, skip this e2e test instance")
			return
		}
		pickupNode := nodes.Items[0]
		ginkgo.By(fmt.Sprintf("pick up node %s and make it under disk pressure", pickupNode.Name))
		index := 0
		rootDir := "/var/lib/kubelet"
		timeout := 5 * time.Minute
		poll := 5 * time.Second
		err = wait.PollUntilContextTimeout(ctx, poll, timeout, true, func(ctx context.Context) (bool, error) {
			cmd := fmt.Sprintf("dd if=/dev/zero of=%s/tmpfile-%s bs=1G count=128", rootDir, index)
			e2essh.NodeExec(ctx, pickupNode.Name, cmd, framework.TestContext.Provider)
			node, _ := nodeClient.Get(ctx, pickupNode.Name, metav1.GetOptions{})
			for _, condition := range node.Status.Conditions {
				if condition.Type == v1.NodeDiskPressure {
					if condition.Status == v1.ConditionTrue {
						return true, nil
					}
					break
				}
			}
			index++
			return false, nil
		})
		framework.ExpectNoError(err)

		ginkgo.DeferCleanup(func(ctx context.Context) error {
			ginkgo.By("remove all dummy files")
			cmd := fmt.Sprintf("rm -f %s/tmpfile-*", rootDir)
			_, err := e2essh.NodeExec(ctx, pickupNode.Name, cmd, framework.TestContext.Provider)
			return err
		})

		ginkgo.By("start up a go routine to watch node")
		go func() {
			watchChan, err := nodeClient.Watch(ctx, metav1.ListOptions{Watch: true, FieldSelector: fields.OneTermNotEqualSelector("metadata.name", pickupNode.Name).String()})
			framework.ExpectNoError(err)
			for {
				select {
				case event, closed := <-watchChan.ResultChan():
					if !closed {
						framework.ExpectNoError(fmt.Errorf("watch stream is closed unexpectedly"))
					}
					node, ok := event.Object.(*v1.Node)
					if !ok {
						framework.ExpectNoError(fmt.Errorf("failed to get correct object type from watch channel"))
					}
					for _, condition := range node.Status.Conditions {
						if condition.Type == v1.NodeDiskPressure {
							if condition.Status == v1.ConditionTrue {
								framework.ExpectNoError(fmt.Errorf("node taint has been wiped which is not expected"))
							}
						}
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		ginkgo.By(fmt.Sprintf("node %s has been under disk pressure, restart it", pickupNode.Name))
		err = wait.PollUntilContextTimeout(ctx, poll, timeout, true, func(ctx context.Context) (bool, error) {
			cmd := fmt.Sprintf("systemctl restart kubelet")
			result, err := e2essh.NodeExec(ctx, pickupNode.Name, cmd, framework.TestContext.Provider)
			ok := result.Code == 0 && len(result.Stdout) == 0 && len(result.Stderr) == 0
			if !ok || err != nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err)

		time.Sleep(time.Minute)
	})
})

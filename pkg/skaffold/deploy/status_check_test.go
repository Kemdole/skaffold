/*
Copyright 2019 The Skaffold Authors

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

package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekubeclientset "k8s.io/client-go/kubernetes/fake"
	utilpointer "k8s.io/utils/pointer"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/GoogleContainerTools/skaffold/testutil"
)

func TestGetDeployments(t *testing.T) {
	labeller := NewLabeller("")
	tests := []struct {
		description string
		deps        []*appsv1.Deployment
		expected    map[string]time.Duration
		shouldErr   bool
	}{
		{
			description: "multiple deployments in same namespace",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
							"random":   "foo",
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(10)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep2",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(20)},
				},
			},
			expected: map[string]time.Duration{"dep1": time.Duration(10) * time.Second, "dep2": time.Duration(20) * time.Second},
		}, {
			description: "command flag deadline is less than deployment spec.",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
							"random":   "foo",
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(300)},
				},
			},
			expected: map[string]time.Duration{"dep1": time.Duration(200) * time.Second},
		}, {
			description: "multiple deployments with no progress deadline set",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(100)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep2",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
						},
					},
				},
			},
			expected: map[string]time.Duration{"dep1": time.Duration(100) * time.Second,
				"dep2": time.Duration(200) * time.Second},
		},
		{
			description: "no deployments",
			expected:    map[string]time.Duration{},
		},
		{
			description: "multiple deployments in different namespaces",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(100)},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep2",
						Namespace: "test1",
						Labels: map[string]string{
							RunIDLabel: labeller.runID,
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(100)},
				},
			},
			expected: map[string]time.Duration{"dep1": time.Duration(100) * time.Second},
		},
		{
			description: "deployment in correct namespace but not deployed by skaffold",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							"some-other-tool": "helm",
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(100)},
				},
			},
			expected: map[string]time.Duration{},
		},
		{
			description: "deployment in correct namespace deployed by skaffold but different run",
			deps: []*appsv1.Deployment{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dep1",
						Namespace: "test",
						Labels: map[string]string{
							RunIDLabel: "9876-6789",
						},
					},
					Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: utilpointer.Int32Ptr(100)},
				},
			},
			expected: map[string]time.Duration{},
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			objs := make([]runtime.Object, len(test.deps))
			for i, dep := range test.deps {
				objs[i] = dep
			}
			client := fakekubeclientset.NewSimpleClientset(objs...)
			actual, err := getDeployments(client, "test", labeller, time.Duration(200)*time.Second)
			t.CheckErrorAndDeepEqual(test.shouldErr, err, test.expected, actual)
		})
	}
}

func TestPollDeploymentRolloutStatus(t *testing.T) {
	rolloutCmd := "kubectl --context kubecontext --namespace test rollout status deployment dep --watch=false"
	tests := []struct {
		description string
		commands    util.Command
		duration    int
		shouldErr   bool
	}{
		{
			description: "rollout returns success",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"dep successfully rolled out",
			),
			duration: 50,
		}, {
			description: "rollout returns error in the first attempt",
			commands: testutil.CmdRunOutErr(
				rolloutCmd, "could not find",
				errors.New("deployment.apps/dep could not be found"),
			),
			shouldErr: true,
			duration:  50,
		}, {
			description: "rollout returns did not stabilize within the given timeout",
			commands: testutil.
				CmdRunOut(rolloutCmd, "Waiting for rollout to finish: 1 of 3 updated replicas are available...").
				AndRunOut(rolloutCmd, "Waiting for rollout to finish: 1 of 3 updated replicas are available...").
				AndRunOut(rolloutCmd, "Waiting for rollout to finish: 2 of 3 updated replicas are available..."),
			duration:  20,
			shouldErr: true,
		},
	}
	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			t.Override(&defaultPollPeriodInMilliseconds, 10)
			t.Override(&util.DefaultExecCommand, test.commands)

			cli := &kubectl.CLI{KubeContext: testKubeContext, Namespace: "test"}
			err := pollDeploymentRolloutStatus(context.Background(), cli, "dep", time.Duration(test.duration)*time.Millisecond)
			t.CheckError(test.shouldErr, err)
		})
	}
}

func TestGetDeployStatus(t *testing.T) {
	tests := []struct {
		description    string
		deps           map[string]interface{}
		expectedErrMsg []string
		shouldErr      bool
	}{
		{
			description: "one error",
			deps: map[string]interface{}{
				"dep1": "SUCCESS",
				"dep2": fmt.Errorf("could not return within default timeout"),
			},
			expectedErrMsg: []string{"deployment dep2 failed due to could not return within default timeout"},
			shouldErr:      true,
		},
		{
			description: "no error",
			deps: map[string]interface{}{
				"dep1": "SUCCESS",
				"dep2": "RUNNING",
			},
		},
		{
			description: "multiple errors",
			deps: map[string]interface{}{
				"dep1": "SUCCESS",
				"dep2": fmt.Errorf("could not return within default timeout"),
				"dep3": fmt.Errorf("ERROR"),
			},
			expectedErrMsg: []string{"deployment dep2 failed due to could not return within default timeout",
				"deployment dep3 failed due to ERROR"},
			shouldErr: true,
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			syncMap := &sync.Map{}
			for k, v := range test.deps {
				syncMap.Store(k, v)
			}
			err := getSkaffoldDeployStatus(syncMap)
			t.CheckError(test.shouldErr, err)
			for _, msg := range test.expectedErrMsg {
				t.CheckErrorContains(msg, err)
			}
		})
	}
}

func TestGetRollOutStatus(t *testing.T) {
	rolloutCmd := "kubectl --context kubecontext --namespace test rollout status deployment dep --watch=false"
	tests := []struct {
		description string
		commands    util.Command
		expected    string
		shouldErr   bool
	}{
		{
			description: "some output",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"Waiting for replicas to be available",
			),
			expected: "Waiting for replicas to be available",
		},
		{
			description: "no output",
			commands: testutil.CmdRunOut(
				rolloutCmd,
				"",
			),
		},
		{
			description: "rollout status error",
			commands: testutil.CmdRunOutErr(
				rolloutCmd,
				"",
				errors.New("error"),
			),
			shouldErr: true,
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			t.Override(&util.DefaultExecCommand, test.commands)

			cli := &kubectl.CLI{KubeContext: testKubeContext, Namespace: "test"}
			actual, err := getRollOutStatus(context.Background(), cli, "dep")

			t.CheckErrorAndDeepEqual(test.shouldErr, err, test.expected, actual)
		})
	}
}

func TestPrintSummaryStatus(t *testing.T) {
	tests := []struct {
		description string
		pending     int32
		err         error
		expected    string
	}{
		{
			description: "no deployment left and current is in success",
			pending:     0,
			err:         nil,
			expected:    " - deployment/dep is ready.\n",
		},
		{
			description: "no deployment left and current is in error",
			pending:     0,
			err:         errors.New("context deadline expired"),
			expected:    " - deployment/dep failed. Error: context deadline expired.\n",
		},
		{
			description: "more than 1 deployment left and current is in success",
			pending:     4,
			err:         nil,
			expected:    " - deployment/dep is ready. [4/10 deployment(s) still pending]\n",
		},
		{
			description: "more than 1 deployment left and current is in error",
			pending:     8,
			err:         errors.New("context deadline expired"),
			expected:    " - deployment/dep failed. [8/10 deployment(s) still pending] Error: context deadline expired.\n",
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.description, func(t *testutil.T) {
			out := new(bytes.Buffer)
			c := &counter{
				total:   10,
				pending: test.pending,
			}
			printStatusCheckSummary("dep", c, int(test.pending), test.err, out)
			t.CheckDeepEqual(test.expected, out.String())
		})
	}
}

func TestPrintPendingRace(t *testing.T) {
	testutil.Run(t, "race test", func(t *testutil.T) {
		c := newCounter(100000)

		numSeen := sync.Map{}
		for i := 0; i < c.total; i++ {
			go func() {
				pending := c.markProcessed()
				str := c.getPendingMessage(pending)
				var reportedPending int
				pattern := regexp.MustCompile(".*\\[(\\d+)/.*")
				matches := pattern.FindStringSubmatch(str)
				if len(matches) < 2 {
					reportedPending = 0
				} else {
					var err error
					reportedPending, err = strconv.Atoi(matches[1])
					t.CheckNoError(err)
				}
				if _, ok := numSeen.Load(reportedPending); ok {
					t.Errorf("race detected: saw %d reported twice.", reportedPending)
					t.FailNow()
				}
				numSeen.Store(reportedPending, struct{}{})
			}()
		}
	})
}

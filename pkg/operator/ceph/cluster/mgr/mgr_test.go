/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package mgr

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/apis/rook.io"
	"github.com/rook/rook/pkg/client/clientset/versioned/scheme"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	cephver "github.com/rook/rook/pkg/operator/ceph/version"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	testopk8s "github.com/rook/rook/pkg/operator/k8sutil/test"
	testop "github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tevino/abool"
	apps "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStartMgr(t *testing.T) {
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("Execute: %s %v", command, args)
			if args[0] == "mgr" && args[1] == "stat" {
				return `{"active_name": "a"}`, nil
			}
			return "{\"key\":\"mysecurekey\"}", nil
		},
	}
	waitForDeploymentToStart = func(clusterdContext *clusterd.Context, deployment *apps.Deployment) error {
		logger.Infof("simulated mgr deployment starting")
		return nil
	}

	clientset := testop.New(t, 3)
	configDir, _ := ioutil.TempDir("", "")
	scheme := scheme.Scheme
	err := policyv1.AddToScheme(scheme)
	assert.NoError(t, err)
	err = policyv1beta1.AddToScheme(scheme)
	assert.NoError(t, err)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects().Build()

	defer os.RemoveAll(configDir)
	ctx := &clusterd.Context{
		Executor:                   executor,
		ConfigDir:                  configDir,
		Clientset:                  clientset,
		RequestCancelOrchestration: abool.New(),
		Client:                     cl}
	ownerInfo := cephclient.NewMinimumOwnerInfo(t)
	clusterInfo := &cephclient.ClusterInfo{Namespace: "ns", FSID: "myfsid", OwnerInfo: ownerInfo, CephVersion: cephver.CephVersion{Major: 16, Minor: 2, Build: 5}}
	clusterInfo.SetName("test")
	clusterSpec := cephv1.ClusterSpec{
		Annotations:        map[rook.KeyType]rook.Annotations{cephv1.KeyMgr: {"my": "annotation"}},
		Labels:             map[rook.KeyType]rook.Labels{cephv1.KeyMgr: {"my-label-key": "value"}},
		Dashboard:          cephv1.DashboardSpec{Enabled: true, SSL: true},
		Mgr:                cephv1.MgrSpec{Count: 1},
		PriorityClassNames: map[rook.KeyType]string{cephv1.KeyMgr: "my-priority-class"},
		DataDirHostPath:    "/var/lib/rook/",
	}
	c := New(ctx, clusterInfo, clusterSpec, "myversion")
	defer os.RemoveAll(c.spec.DataDirHostPath)

	// start a basic service
	err = c.Start()
	assert.Nil(t, err)
	validateStart(t, c)
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	c.spec.Dashboard.URLPrefix = "/test"
	c.spec.Dashboard.Port = 12345
	err = c.Start()
	assert.Nil(t, err)
	validateStart(t, c)
	assert.ElementsMatch(t, []string{"rook-ceph-mgr-a"}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// starting with more replicas
	c.spec.Mgr.Count = 2
	c.spec.Dashboard.Enabled = false
	// delete the previous mgr since the mocked test won't update the existing one
	err = c.context.Clientset.AppsV1().Deployments(c.clusterInfo.Namespace).Delete(context.TODO(), "rook-ceph-mgr-a", metav1.DeleteOptions{})
	assert.Nil(t, err)
	err = c.Start()
	assert.Nil(t, err)
	validateStart(t, c)

	c.spec.Mgr.Count = 1
	c.spec.Dashboard.Enabled = false
	// clean the previous deployments
	err = c.context.Clientset.AppsV1().Deployments(c.clusterInfo.Namespace).Delete(context.TODO(), "rook-ceph-mgr-a", metav1.DeleteOptions{})
	assert.Nil(t, err)
	assert.Nil(t, err)
	err = c.Start()
	assert.Nil(t, err)
	validateStart(t, c)
}

func validateStart(t *testing.T, c *Cluster) {
	mgrNames := []string{"a", "b"}
	for i := 0; i < c.spec.Mgr.Count; i++ {
		logger.Infof("Looking for cephmgr replica %d", i)
		daemonName := mgrNames[i]
		d, err := c.context.Clientset.AppsV1().Deployments(c.clusterInfo.Namespace).Get(context.TODO(), fmt.Sprintf("rook-ceph-mgr-%s", daemonName), metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, map[string]string{"my": "annotation"}, d.Spec.Template.Annotations)
		assert.Contains(t, d.Spec.Template.Labels, "my-label-key")
		assert.Equal(t, "my-priority-class", d.Spec.Template.Spec.PriorityClassName)
		if c.spec.Mgr.Count == 1 {
			assert.Equal(t, 1, len(d.Spec.Template.Spec.Containers))
		} else {
			// The sidecar container is only there when multiple mgrs are enabled
			assert.Equal(t, 2, len(d.Spec.Template.Spec.Containers))
			assert.Equal(t, "watch-active", d.Spec.Template.Spec.Containers[1].Name)
		}
	}

	// verify we have exactly the expected number of deployments and not extra
	// the expected deployments were already retrieved above, but now we check for no extra deployments
	options := metav1.ListOptions{LabelSelector: "app=rook-ceph-mgr"}
	deployments, err := c.context.Clientset.AppsV1().Deployments(c.clusterInfo.Namespace).List(context.TODO(), options)
	assert.NoError(t, err)
	assert.Equal(t, c.spec.Mgr.Count, len(deployments.Items))

	validateServices(t, c)
}

func validateServices(t *testing.T, c *Cluster) {
	_, err := c.context.Clientset.CoreV1().Services(c.clusterInfo.Namespace).Get(context.TODO(), "rook-ceph-mgr", metav1.GetOptions{})
	assert.Nil(t, err)

	ds, err := c.context.Clientset.CoreV1().Services(c.clusterInfo.Namespace).Get(context.TODO(), "rook-ceph-mgr-dashboard", metav1.GetOptions{})
	if c.spec.Dashboard.Enabled {
		assert.NoError(t, err)
		if c.spec.Dashboard.Port == 0 {
			// port=0 -> default port
			assert.Equal(t, ds.Spec.Ports[0].Port, int32(dashboardPortHTTPS))
		} else {
			// non-zero ports are configured as-is
			assert.Equal(t, ds.Spec.Ports[0].Port, int32(c.spec.Dashboard.Port))
		}
	} else {
		assert.True(t, errors.IsNotFound(err))
	}
}

func TestMgrSidecarReconcile(t *testing.T) {
	activeMgr := "a"
	calledMgrStat := false
	calledMgrDump := false
	spec := cephv1.ClusterSpec{
		Mgr: cephv1.MgrSpec{Count: 1},
		Dashboard: cephv1.DashboardSpec{
			Enabled: true,
			Port:    7000,
		},
	}
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if args[1] == "dump" {
				calledMgrDump = true
			} else if args[1] == "stat" {
				calledMgrStat = true
			}
			return fmt.Sprintf(`{"active_name":"%s"}`, activeMgr), nil
		},
	}
	clientset := testop.New(t, 3)
	configDir, _ := ioutil.TempDir("", "")
	defer os.RemoveAll(configDir)
	ctx := &clusterd.Context{
		Executor:  executor,
		ConfigDir: configDir,
		Clientset: clientset,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfo(t)
	clusterInfo := &cephclient.ClusterInfo{Namespace: "ns", OwnerInfo: ownerInfo}
	clusterInfo.SetName("test")
	c := &Cluster{spec: spec, context: ctx, clusterInfo: clusterInfo}

	// Update services according to the active mgr
	clusterInfo.CephVersion = cephver.CephVersion{Major: 15, Minor: 2, Build: 0}
	err := c.ReconcileActiveMgrServices(activeMgr)
	assert.NoError(t, err)
	assert.False(t, calledMgrStat)
	assert.True(t, calledMgrDump)
	validateServices(t, c)
	validateServiceMatches(t, c, "a")

	// nothing is created or updated when the requested mgr is not the active mgr
	calledMgrDump = false
	clusterInfo.CephVersion = cephver.CephVersion{Major: 16, Minor: 2, Build: 5}
	err = c.ReconcileActiveMgrServices("b")
	assert.NoError(t, err)
	assert.True(t, calledMgrStat)
	assert.False(t, calledMgrDump)
	_, err = c.context.Clientset.CoreV1().Services(c.clusterInfo.Namespace).Get(context.TODO(), "rook-ceph-mgr", metav1.GetOptions{})
	assert.True(t, errors.IsNotFound(err))

	// nothing is updated when the requested mgr is not the active mgr
	activeMgr = "b"
	err = c.ReconcileActiveMgrServices("b")
	assert.NoError(t, err)
	validateServices(t, c)
	validateServiceMatches(t, c, "b")
}

func validateServiceMatches(t *testing.T, c *Cluster, expectedActive string) {
	// The service labels should match the active mgr
	svc, err := c.context.Clientset.CoreV1().Services(c.clusterInfo.Namespace).Get(context.TODO(), "rook-ceph-mgr", metav1.GetOptions{})
	assert.NoError(t, err)
	matchDaemon, ok := svc.Spec.Selector["ceph_daemon_id"]
	assert.True(t, ok)
	assert.Equal(t, expectedActive, matchDaemon)

	// clean up the service for the next test
	err = c.context.Clientset.CoreV1().Services(c.clusterInfo.Namespace).Delete(context.TODO(), "rook-ceph-mgr", metav1.DeleteOptions{})
	assert.NoError(t, err)
}

func TestConfigureModules(t *testing.T) {
	modulesEnabled := 0
	modulesDisabled := 0
	configSettings := map[string]string{}
	lastModuleConfigured := ""
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if command == "ceph" && len(args) > 3 {
				if args[0] == "mgr" && args[1] == "module" {
					if args[2] == "enable" {
						modulesEnabled++
					}
					if args[2] == "disable" {
						modulesDisabled++
					}
					lastModuleConfigured = args[3]
				}
				if args[0] == "config" && args[1] == "set" && args[2] == "global" {
					configSettings[args[3]] = args[4]
				}
			}
			return "", nil //return "{\"key\":\"mysecurekey\"}", nil
		},
	}

	clientset := testop.New(t, 3)
	context := &clusterd.Context{Executor: executor, Clientset: clientset}
	clusterInfo := &cephclient.ClusterInfo{Namespace: "ns"}
	c := &Cluster{
		context:     context,
		clusterInfo: clusterInfo,
	}

	// one module without any special configuration
	c.spec.Mgr.Modules = []cephv1.Module{
		{Name: "mymodule", Enabled: true},
	}
	assert.NoError(t, c.configureMgrModules())
	assert.Equal(t, 1, modulesEnabled)
	assert.Equal(t, 0, modulesDisabled)
	assert.Equal(t, "mymodule", lastModuleConfigured)

	// one module that has a min version that is not met
	c.spec.Mgr.Modules = []cephv1.Module{
		{Name: "pg_autoscaler", Enabled: true},
	}

	// one module that has a min version that is met
	c.spec.Mgr.Modules = []cephv1.Module{
		{Name: "pg_autoscaler", Enabled: true},
	}
	c.clusterInfo.CephVersion = cephver.CephVersion{Major: 14}
	modulesEnabled = 0
	assert.NoError(t, c.configureMgrModules())
	assert.Equal(t, 1, modulesEnabled)
	assert.Equal(t, 0, modulesDisabled)
	assert.Equal(t, "pg_autoscaler", lastModuleConfigured)
	assert.Equal(t, 2, len(configSettings))
	assert.Equal(t, "on", configSettings["osd_pool_default_pg_autoscale_mode"])
	assert.Equal(t, "0", configSettings["mon_pg_warn_min_per_osd"])

	// disable the module
	modulesEnabled = 0
	lastModuleConfigured = ""
	configSettings = map[string]string{}
	c.spec.Mgr.Modules[0].Enabled = false
	assert.NoError(t, c.configureMgrModules())
	assert.Equal(t, 0, modulesEnabled)
	assert.Equal(t, 1, modulesDisabled)
	assert.Equal(t, "pg_autoscaler", lastModuleConfigured)
	assert.Equal(t, 0, len(configSettings))
}

func TestMgrDaemons(t *testing.T) {
	spec := cephv1.ClusterSpec{
		Mgr: cephv1.MgrSpec{Count: 1},
	}
	c := &Cluster{spec: spec}
	daemons := c.getDaemonIDs()
	require.Equal(t, 1, len(daemons))
	assert.Equal(t, "a", daemons[0])

	c.spec.Mgr.Count = 2
	daemons = c.getDaemonIDs()
	require.Equal(t, 2, len(daemons))
	assert.Equal(t, "a", daemons[0])
	assert.Equal(t, "b", daemons[1])
}

func TestApplyMonitoringLabels(t *testing.T) {
	clusterSpec := cephv1.ClusterSpec{
		Labels: cephv1.LabelsSpec{},
	}
	c := &Cluster{spec: clusterSpec}
	sm := &monitoringv1.ServiceMonitor{Spec: monitoringv1.ServiceMonitorSpec{
		Endpoints: []monitoringv1.Endpoint{{}}}}

	// Service Monitor RelabelConfigs updated when 'rook.io/managedBy' monitoring label is found
	monitoringLabels := cephv1.LabelsSpec{
		cephv1.KeyMonitoring: map[string]string{
			"rook.io/managedBy": "storagecluster"},
	}
	c.spec.Labels = monitoringLabels
	applyMonitoringLabels(c, sm)
	fmt.Printf("Hello1")
	assert.Equal(t, "managedBy", sm.Spec.Endpoints[0].RelabelConfigs[0].TargetLabel)
	assert.Equal(t, "storagecluster", sm.Spec.Endpoints[0].RelabelConfigs[0].Replacement)

	// Service Monitor RelabelConfigs not updated when the required monitoring label is not found
	monitoringLabels = cephv1.LabelsSpec{
		cephv1.KeyMonitoring: map[string]string{
			"wrongLabelKey": "storagecluster"},
	}
	c.spec.Labels = monitoringLabels
	sm.Spec.Endpoints[0].RelabelConfigs = nil
	applyMonitoringLabels(c, sm)
	assert.Nil(t, sm.Spec.Endpoints[0].RelabelConfigs)

	// Service Monitor RelabelConfigs not updated when no monitoring labels are found
	c.spec.Labels = cephv1.LabelsSpec{}
	sm.Spec.Endpoints[0].RelabelConfigs = nil
	applyMonitoringLabels(c, sm)
	assert.Nil(t, sm.Spec.Endpoints[0].RelabelConfigs)
}

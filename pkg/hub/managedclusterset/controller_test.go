package managedclusterset

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/events/eventstesting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	clusterfake "open-cluster-management.io/api/client/cluster/clientset/versioned/fake"
	clusterinformers "open-cluster-management.io/api/client/cluster/informers/externalversions"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	testinghelpers "open-cluster-management.io/registration/pkg/helpers/testing"
)

func TestSyncClusterSet(t *testing.T) {
	cases := []struct {
		name                   string
		clusterSetName         string
		existingClusterSet     *clusterv1alpha1.ManagedClusterSet
		existingClusters       []*clusterv1.ManagedCluster
		expectedClusterSetsMap map[string]string
		validateActions        func(t *testing.T, actions []clienttesting.Action)
	}{
		{
			name:           "sync a deleted cluster set",
			clusterSetName: "mcs1",
			validateActions: func(t *testing.T, actions []clienttesting.Action) {
				testinghelpers.AssertNoActions(t, actions)
			},
		},
		{
			name:               "sync a deleting cluster set",
			clusterSetName:     "mcs1",
			existingClusterSet: newManagedClusterSet("mcs1", true),
			validateActions: func(t *testing.T, actions []clienttesting.Action) {
				testinghelpers.AssertNoActions(t, actions)
			},
		},
		{
			name:               "sync a empty cluster set",
			clusterSetName:     "mcs1",
			existingClusterSet: newManagedClusterSet("mcs1", false),
			validateActions: func(t *testing.T, actions []clienttesting.Action) {
				testinghelpers.AssertActions(t, actions, "update")
				clusterSet := actions[0].(clienttesting.UpdateAction).GetObject().(*clusterv1alpha1.ManagedClusterSet)
				if !hasCondition(
					clusterSet.Status.Conditions,
					clusterv1alpha1.ManagedClusterSetConditionEmpty,
					metav1.ConditionTrue,
					"NoClusterMatched",
					"No ManagedCluster selected") {
					t.Errorf("expected conditon is not found: %v", clusterSet.Status.Conditions)
				}
			},
		},
		{
			name:               "sync a cluster set",
			clusterSetName:     "mcs1",
			existingClusterSet: newManagedClusterSet("mcs1", false),
			existingClusters: []*clusterv1.ManagedCluster{
				newManagedCluster("cluster1", "mcs1"),
				newManagedCluster("cluster2", "mcs2"),
				newManagedCluster("cluster3", "mcs1"),
			},
			expectedClusterSetsMap: map[string]string{
				"cluster1": "mcs1",
				"cluster3": "mcs1",
			},
			validateActions: func(t *testing.T, actions []clienttesting.Action) {
				testinghelpers.AssertActions(t, actions, "update")
				clusterSet := actions[0].(clienttesting.UpdateAction).GetObject().(*clusterv1alpha1.ManagedClusterSet)
				if !hasCondition(
					clusterSet.Status.Conditions,
					clusterv1alpha1.ManagedClusterSetConditionEmpty,
					metav1.ConditionFalse,
					"ClustersSelected",
					"2 ManagedClusters selected") {
					t.Errorf("expected conditon is not found: %v", clusterSet.Status.Conditions)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects := []runtime.Object{}
			for _, cluster := range c.existingClusters {
				objects = append(objects, cluster)
			}
			if c.existingClusterSet != nil {
				objects = append(objects, c.existingClusterSet)
			}

			clusterClient := clusterfake.NewSimpleClientset(objects...)

			informerFactory := clusterinformers.NewSharedInformerFactory(clusterClient, 5*time.Minute)
			for _, cluster := range c.existingClusters {
				informerFactory.Cluster().V1().ManagedClusters().Informer().GetStore().Add(cluster)
			}
			if c.existingClusterSet != nil {
				informerFactory.Cluster().V1alpha1().ManagedClusterSets().Informer().GetStore().Add(c.existingClusterSet)
			}

			ctrl := managedClusterSetController{
				clusterClient:    clusterClient,
				clusterLister:    informerFactory.Cluster().V1().ManagedClusters().Lister(),
				clusterSetLister: informerFactory.Cluster().V1alpha1().ManagedClusterSets().Lister(),
				eventRecorder:    eventstesting.NewTestingEventRecorder(t),
				clusterSetsMap:   map[string]string{},
			}

			syncErr := ctrl.sync(context.Background(), testinghelpers.NewFakeSyncContext(t, c.clusterSetName))
			if syncErr != nil {
				t.Errorf("unexpected err: %v", syncErr)
			}

			c.validateActions(t, clusterClient.Actions())

			if c.expectedClusterSetsMap == nil {
				c.expectedClusterSetsMap = map[string]string{}
			}

			if !reflect.DeepEqual(ctrl.clusterSetsMap, c.expectedClusterSetsMap) {
				t.Errorf("expected mappings %v, but got %v", c.expectedClusterSetsMap, ctrl.clusterSetsMap)
			}
		})
	}
}

func newManagedCluster(name, clusterSet string) *clusterv1.ManagedCluster {
	cluster := &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	if len(clusterSet) > 0 {
		cluster.Labels = map[string]string{
			clusterSetLabel: clusterSet,
		}
	}

	return cluster
}

func newManagedClusterSet(name string, terminating bool) *clusterv1alpha1.ManagedClusterSet {
	clusterSet := &clusterv1alpha1.ManagedClusterSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if terminating {
		now := metav1.Now()
		clusterSet.DeletionTimestamp = &now
	}

	return clusterSet
}

func hasCondition(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) bool {
	for _, condition := range conditions {
		if condition.Type != conditionType {
			continue
		}
		if condition.Status != status {
			continue
		}
		if condition.Reason != reason {
			continue
		}
		if condition.Message != message {
			continue
		}
		return true
	}
	return false
}

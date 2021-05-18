package helpers

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"

	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonv1alpha1client "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterclientset "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"open-cluster-management.io/registration/pkg/hub/managedcluster/bindata"

	"github.com/openshift/api"
	"github.com/openshift/library-go/pkg/assets"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
	errorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

var (
	genericScheme = runtime.NewScheme()
	genericCodecs = serializer.NewCodecFactory(genericScheme)
	genericCodec  = genericCodecs.UniversalDeserializer()
)

func init() {
	utilruntime.Must(api.InstallKube(genericScheme))
}

type UpdateManagedClusterStatusFunc func(status *clusterv1.ManagedClusterStatus) error

func UpdateManagedClusterStatus(
	ctx context.Context,
	client clusterclientset.Interface,
	spokeClusterName string,
	updateFuncs ...UpdateManagedClusterStatusFunc) (*clusterv1.ManagedClusterStatus, bool, error) {
	updated := false
	var updatedManagedClusterStatus *clusterv1.ManagedClusterStatus

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		managedCluster, err := client.ClusterV1().ManagedClusters().Get(ctx, spokeClusterName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldStatus := &managedCluster.Status

		newStatus := oldStatus.DeepCopy()
		for _, update := range updateFuncs {
			if err := update(newStatus); err != nil {
				return err
			}
		}
		if equality.Semantic.DeepEqual(oldStatus, newStatus) {
			// We return the newStatus which is a deep copy of oldStatus but with all update funcs applied.
			updatedManagedClusterStatus = newStatus
			return nil
		}

		managedCluster.Status = *newStatus
		updatedManagedCluster, err := client.ClusterV1().ManagedClusters().UpdateStatus(ctx, managedCluster, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		updatedManagedClusterStatus = &updatedManagedCluster.Status
		updated = err == nil
		return err
	})

	return updatedManagedClusterStatus, updated, err
}

func UpdateManagedClusterConditionFn(cond metav1.Condition) UpdateManagedClusterStatusFunc {
	return func(oldStatus *clusterv1.ManagedClusterStatus) error {
		meta.SetStatusCondition(&oldStatus.Conditions, cond)
		return nil
	}
}

type UpdateManagedClusterAddOnStatusFunc func(status *addonv1alpha1.ManagedClusterAddOnStatus) error

func UpdateManagedClusterAddOnStatus(
	ctx context.Context,
	client addonv1alpha1client.Interface,
	addOnNamespace, addOnName string,
	updateFuncs ...UpdateManagedClusterAddOnStatusFunc) (*addonv1alpha1.ManagedClusterAddOnStatus, bool, error) {
	updated := false
	var updatedAddOnStatus *addonv1alpha1.ManagedClusterAddOnStatus

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		addOn, err := client.AddonV1alpha1().ManagedClusterAddOns(addOnNamespace).Get(ctx, addOnName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldStatus := &addOn.Status

		newStatus := oldStatus.DeepCopy()
		for _, update := range updateFuncs {
			if err := update(newStatus); err != nil {
				return err
			}
		}
		if equality.Semantic.DeepEqual(oldStatus, newStatus) {
			// We return the newStatus which is a deep copy of oldStatus but with all update funcs applied.
			updatedAddOnStatus = newStatus
			return nil
		}

		addOn.Status = *newStatus
		updatedAddOn, err := client.AddonV1alpha1().ManagedClusterAddOns(addOnNamespace).UpdateStatus(ctx, addOn, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		updatedAddOnStatus = &updatedAddOn.Status
		updated = err == nil
		return err
	})

	return updatedAddOnStatus, updated, err
}

func UpdateManagedClusterAddOnStatusFn(cond metav1.Condition) UpdateManagedClusterAddOnStatusFunc {
	return func(oldStatus *addonv1alpha1.ManagedClusterAddOnStatus) error {
		meta.SetStatusCondition(&oldStatus.Conditions, cond)
		return nil
	}
}

// Check whether a CSR is in terminal state
func IsCSRInTerminalState(status *certificatesv1.CertificateSigningRequestStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == certificatesv1.CertificateApproved {
			return true
		}
		if c.Type == certificatesv1.CertificateDenied {
			return true
		}
	}
	return false
}

// IsValidHTTPSURL validate whether a URL is https URL
func IsValidHTTPSURL(serverURL string) bool {
	if serverURL == "" {
		return false
	}

	parsedServerURL, err := url.Parse(serverURL)
	if err != nil {
		return false
	}

	if parsedServerURL.Scheme != "https" {
		return false
	}

	return true
}

// CleanUpManagedClusterManifests clean up managed cluster resources from its manifest files
func CleanUpManagedClusterManifests(
	ctx context.Context,
	client kubernetes.Interface,
	recorder events.Recorder,
	assetFunc resourceapply.AssetFunc,
	files ...string) error {
	errs := []error{}
	for _, file := range files {
		objectRaw, err := assetFunc(file)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		object, _, err := genericCodec.Decode(objectRaw, nil, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch t := object.(type) {
		case *corev1.Namespace:
			err = client.CoreV1().Namespaces().Delete(ctx, t.Name, metav1.DeleteOptions{})
		case *rbacv1.Role:
			err = client.RbacV1().Roles(t.Namespace).Delete(ctx, t.Name, metav1.DeleteOptions{})
		case *rbacv1.RoleBinding:
			err = client.RbacV1().RoleBindings(t.Namespace).Delete(ctx, t.Name, metav1.DeleteOptions{})
		case *rbacv1.ClusterRole:
			err = client.RbacV1().ClusterRoles().Delete(ctx, t.Name, metav1.DeleteOptions{})
		case *rbacv1.ClusterRoleBinding:
			err = client.RbacV1().ClusterRoleBindings().Delete(ctx, t.Name, metav1.DeleteOptions{})
		default:
			err = fmt.Errorf("unhandled type %T", object)
		}
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		gvk := resourcehelper.GuessObjectGroupVersionKind(object)
		recorder.Eventf(fmt.Sprintf("ManagedCluster%sDeleted", gvk.Kind), "Deleted %s", resourcehelper.FormatResourceForCLIWithNamespace(object))
	}
	return errorhelpers.NewMultiLineAggregate(errs)
}

// CleanUpGroupFromClusterRoleBindings search all clusterrolebindings for managed cluster group and remove the subject entry
// or delete the clusterrolebinding if it's the only subject.
func CleanUpGroupFromClusterRoleBindings(
	ctx context.Context,
	client kubernetes.Interface,
	recorder events.Recorder,
	managedClusterGroup string) error {
	clusterRoleBindings, err := client.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range clusterRoleBindings.Items {
		clusterRoleBinding := clusterRoleBindings.Items[i]
		subjects := clusterRoleBinding.Subjects
		newSubjects := []rbacv1.Subject{}
		for _, subject := range subjects {
			if subject.Kind == "Group" && subject.Name == managedClusterGroup {
				continue
			}
			newSubjects = append(newSubjects, subject)
		}
		// no other subjects, remove this clusterrolebinding
		if len(newSubjects) == 0 {
			err := client.RbacV1().ClusterRoleBindings().Delete(ctx, clusterRoleBinding.Name, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
			recorder.Eventf("ClusterRoleBindingDeleted", fmt.Sprintf("Deleted ClusterRoleBinding %q", clusterRoleBinding.Name))
			continue
		}
		// there are other subjects, only remove the cluster managed group
		if len(newSubjects) != len(subjects) {
			clusterRoleBinding.Subjects = newSubjects
			_, err := client.RbacV1().ClusterRoleBindings().Update(ctx, &clusterRoleBinding, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			recorder.Eventf("ClusterRoleBindingUpdated", fmt.Sprintf("Updated ClusterRoleBinding %q", clusterRoleBinding.Name))
			continue
		}
	}

	return nil
}

// CleanUpGroupFromRoleBindings search all rolebindings for managed cluster group and remove the subject entry
// or delete the rolebinding if it's the only subject.
func CleanUpGroupFromRoleBindings(
	ctx context.Context,
	client kubernetes.Interface,
	recorder events.Recorder,
	managedClusterGroup string) error {
	roleBindings, err := client.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range roleBindings.Items {
		roleBinding := roleBindings.Items[i]
		subjects := roleBinding.Subjects
		newSubjects := []rbacv1.Subject{}
		for _, subject := range subjects {
			if subject.Kind == "Group" && subject.Name == managedClusterGroup {
				continue
			}
			newSubjects = append(newSubjects, subject)
		}
		// no other subjects, remove this rolebinding
		if len(newSubjects) == 0 {
			err := client.RbacV1().RoleBindings(roleBinding.Namespace).Delete(ctx, roleBinding.Name, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
			recorder.Eventf("RoleBindingDeleted", fmt.Sprintf("Deleted RoleBinding %q/%q", roleBinding.Namespace, roleBinding.Name))
			continue
		}
		// there are other subjects, only remove the cluster managed group
		if len(newSubjects) != len(subjects) {
			roleBinding.Subjects = newSubjects
			_, err := client.RbacV1().RoleBindings(roleBinding.Namespace).Update(ctx, &roleBinding, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			recorder.Eventf("RoleBindingUpdated", fmt.Sprintf("Updated RoleBinding %q/%q", roleBinding.Namespace, roleBinding.Name))
			continue
		}
	}
	return nil
}

func ManagedClusterAssetFn(manifestDir, managedClusterName string) resourceapply.AssetFunc {
	return func(name string) ([]byte, error) {
		config := struct {
			ManagedClusterName string
		}{
			ManagedClusterName: managedClusterName,
		}
		return assets.MustCreateAssetFromTemplate(name, bindata.MustAsset(filepath.Join(manifestDir, name)), config).Data, nil
	}
}

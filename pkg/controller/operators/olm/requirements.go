package olm

import (
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/apis/operators/v1alpha1"
	olmErrors "github.com/operator-framework/operator-lifecycle-manager/pkg/controller/errors"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/install"
)

func (a *Operator) requirementStatus(csv *v1alpha1.ClusterServiceVersion) (met bool, statuses []v1alpha1.RequirementStatus) {
	met = true
	for _, r := range csv.GetAllCRDDescriptions() {
		status := v1alpha1.RequirementStatus{
			Group:   "apiextensions.k8s.io",
			Version: "v1beta1",
			Kind:    "CustomResourceDefinition",
			Name:    r.Name,
		}

		// check if CRD exists - this verifies group, version, and kind, so no need for GVK check via discovery
		crd, err := a.OpClient.ApiextensionsV1beta1Interface().ApiextensionsV1beta1().CustomResourceDefinitions().Get(r.Name, metav1.GetOptions{})
		if err != nil {
			status.Status = v1alpha1.RequirementStatusReasonNotPresent
			met = false
		} else {
			status.Status = v1alpha1.RequirementStatusReasonPresent
			status.UUID = string(crd.GetUID())
		}
		statuses = append(statuses, status)
	}
	for _, r := range csv.GetAllAPIServiceDescriptions() {
		apiName := fmt.Sprintf("%s.%s", r.Version, r.Name)
		status := v1alpha1.RequirementStatus{
			Group:   "apiregistration.k8s.io",
			Version: "v1",
			Kind:    "APIService",
			Name:    apiName,
		}

		// check if GVK exists
		if err := a.isGVKRegistered(r.Name, r.Version, r.Kind); err != nil {
			status.Status = "NotPresent"
			met = false
			statuses = append(statuses, status)
			continue
		}

		// Check if APIService is registered
		apiService, err := a.OpClient.ApiregistrationV1Interface().ApiregistrationV1().APIServices().Get(apiName, metav1.GetOptions{})
		if err != nil {
			status.Status = "NotPresent"
			met = false
			statuses = append(statuses, status)
			continue
		}

		// Check if API is available
		if !a.isAPIServiceAvailable(apiService) {
			status.Status = "NotPresent"
			met = false
		} else {
			status.Status = "Present"
			status.UUID = string(apiService.GetUID())
		}
		statuses = append(statuses, status)
	}

	// Get permission status
	permissionsMet, permissionStatuses := a.permissionStatus(csv)
	log.Infof("CSV %s permission met: %t", csv.GetName(), permissionsMet)
	statuses = append(statuses, permissionStatuses...)
	met = met && permissionsMet

	return
}

func (a *Operator) isGVKRegistered(group, version, kind string) error {
	logger := log.WithFields(log.Fields{
		"group":   group,
		"version": version,
		"kind":    kind,
	})
	groups, err := a.OpClient.KubernetesInterface().Discovery().ServerResources()
	if err != nil {
		logger.WithField("err", err).Info("couldn't query for GVK in api discovery")
		return err
	}
	gv := metav1.GroupVersion{Group: group, Version: version}
	for _, g := range groups {
		if g.GroupVersion == gv.String() {
			for _, r := range g.APIResources {
				if r.Kind == kind {
					return nil
				}
			}
		}
	}
	logger.Info("couldn't find GVK in api discovery")
	return olmErrors.GroupVersionKindNotFoundError{group, version, kind}
}

// permissionStatus checks whether the given CSV's RBAC requirements are met in its namespace
func (a *Operator) permissionStatus(csv *v1alpha1.ClusterServiceVersion) (bool, []v1alpha1.RequirementStatus) {
	// Use a StrategyResolver to unmarshal
	strategyResolver := install.StrategyResolver{}
	strategy, err := strategyResolver.UnmarshalStrategy(csv.Spec.InstallStrategy)
	if err != nil {
		return false, nil
	}

	// Assume the strategy is for a deployment
	strategyDetailsDeployment, ok := strategy.(*install.StrategyDetailsDeployment)
	if !ok {
		return false, nil
	}

	statusesSet := map[string]v1alpha1.RequirementStatus{}
	ruleChecker := install.NewCSVRuleChecker(a.roleLister, a.roleBindingLister, a.clusterRoleLister, a.clusterRoleBindingLister, csv)
	met := true

	checkPermissions := func(permissions []install.StrategyDeploymentPermissions, namespace string) {
		for _, perm := range permissions {
			saName := perm.ServiceAccountName

			var status v1alpha1.RequirementStatus
			if stored, ok := statusesSet[saName]; !ok {
				status = v1alpha1.RequirementStatus{
					Group:      "",
					Version:    "v1",
					Kind:       "ServiceAccount",
					Name:       saName,
					Status:     v1alpha1.RequirementStatusReasonPresent,
					Dependents: []v1alpha1.DependentStatus{},
				}
			} else {
				status = stored
			}

			// Ensure the ServiceAccount exists
			sa, err := a.OpClient.GetServiceAccount(csv.GetNamespace(), perm.ServiceAccountName)
			if err != nil {
				met = false
				status.Status = v1alpha1.RequirementStatusReasonNotPresent
				statusesSet[saName] = status
				continue
			}

			// Check if the PolicyRules are satisfied
			for _, rule := range perm.Rules {
				// TODO(Nick): decide what to do with dependent status here
				dependent := v1alpha1.DependentStatus{
					Group:   "rbac.authorization.k8s.io",
					Kind:    "PolicyRule",
					Version: "v1beta1",
				}

				marshalled, err := json.Marshal(rule)
				if err != nil {
					dependent.Status = v1alpha1.DependentStatusReasonNotSatisfied
					dependent.Message = "rule unmarshallable"
					status.Dependents = append(status.Dependents, dependent)
					continue
				}
				dependent.Message = fmt.Sprintf("rule raw:%s", marshalled)

				satisfied, err := ruleChecker.RuleSatisfied(sa, namespace, rule)
				if err != nil || !satisfied {
					met = false
					dependent.Status = v1alpha1.DependentStatusReasonNotSatisfied
					status.Status = v1alpha1.RequirementStatusReasonPresentNotSatisfied
				} else {
					dependent.Status = v1alpha1.DependentStatusReasonSatisfied
				}

				status.Dependents = append(status.Dependents, dependent)
			}

			statusesSet[saName] = status
		}
	}

	checkPermissions(strategyDetailsDeployment.Permissions, csv.GetNamespace())
	checkPermissions(strategyDetailsDeployment.ClusterPermissions, metav1.NamespaceAll)

	statuses := make([]v1alpha1.RequirementStatus, len(statusesSet))
	for _, status := range statusesSet {
		statuses = append(statuses, status)
	}

	return met, statuses
}

package ingress

import (
	"context"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ensureTrustedCAConfigMap ensures the configmap for the service CA bundle
// exists.  Returns a Boolean indicating whether the configmap exists, the
// configmap if it does exist, and an error value.
func (r *reconciler) ensureTrustedCAConfigMap() (bool, *corev1.ConfigMap, error) {
	wantCM, desired, err := desiredTrustedCAConfigMap()
	if err != nil {
		return false, nil, fmt.Errorf("failed to build configmap: %v", err)
	}

	haveCM, current, err := r.currentTrustedCAConfigMap()
	if err != nil {
		return false, nil, err
	}

	switch {
	case !wantCM && !haveCM:
		return false, nil, nil
	case !wantCM && haveCM:
		if err := r.client.Delete(context.TODO(), current); err != nil {
			if !errors.IsNotFound(err) {
				return true, current, fmt.Errorf("failed to delete configmap: %v", err)
			}
		} else {
			log.Info("deleted configmap", "configmap", current)
		}
		return false, nil, nil
	case wantCM && !haveCM:
		if err := r.client.Create(context.TODO(), desired); err != nil {
			return false, nil, fmt.Errorf("failed to create configmap: %v", err)
		}
		log.Info("created configmap", "configmap", desired)
		return r.currentTrustedCAConfigMap()
	case wantCM && haveCM:
		if updated, err := r.updateTrustedCAConfigMap(current, desired); err != nil {
			return true, current, fmt.Errorf("failed to update configmap: %v", err)
		} else if updated {
			return r.currentTrustedCAConfigMap()
		}
	}

	return true, current, nil
}

// desiredTrustedCAConfigMap returns the desired configmap for the service CA
// bundle.  Returns a Boolean indicating whether a configmap is desired, as well
// as the configmap if one is desired.
func desiredTrustedCAConfigMap() (bool, *corev1.ConfigMap, error) {
	name := controller.TrustedCAConfigMapName()
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"description": "ConfigMap providing service CA bundle.",
			},
			Labels: map[string]string{
				"config.openshift.io/inject-trusted-cabundle": "true",
			},
			Name:      name.Name,
			Namespace: name.Namespace,
		},
	}

	return true, &cm, nil
}

// currentTrustedCAConfigMap returns the current configmap for the service CA
// bundle.  Returns a Boolean indicating whether the configmap existed, the
// configmap if it did exist, and an error value.
func (r *reconciler) currentTrustedCAConfigMap() (bool, *corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := r.client.Get(context.TODO(), controller.TrustedCAConfigMapName(), cm); err != nil {
		if errors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, cm, nil
}

// updateTrustedCAConfigMap updates the configmap for the service CA bundle if
// an update is needed.  In particular, the "inject-cabundle" annotation must be
// set so that the serving cert signer updates the configmap's data with the
// service CA bundle (updateTrustedCAConfigMap itself does not set any data).
// Returns a Boolean indicating whether updateTrustedCAConfigMap updated the
// configmap, and an error value.
func (r *reconciler) updateTrustedCAConfigMap(current, desired *corev1.ConfigMap) (bool, error) {
	if current.Labels != nil && current.Labels["config.openshift.io/inject-trusted-cabundle"] == "true" {
		return false, nil
	}

	updated := current.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	updated.Labels["config.openshift.io/inject-trusted-cabundle"] = "true"
	// Diff before updating because the client may mutate the object.
	diff := cmp.Diff(current, updated, cmpopts.EquateEmpty())
	if err := r.client.Update(context.TODO(), updated); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, err
	}
	log.Info("updated configmap", "namespace", updated.Namespace, "name", updated.Name, "diff", diff)
	return true, nil
}

/*

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

package constrainttemplate

import (
	"context"
	"fmt"
	"reflect"

	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1alpha1"
	opa "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	"github.com/open-policy-agent/gatekeeper/pkg/controller/constraint"
	"github.com/open-policy-agent/gatekeeper/pkg/watch"
	errorpkg "github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	finalizerName = "constrainttemplate.finalizers.gatekeeper.sh"
	ctrlName      = "constrainttemplate-controller"
)

var log = logf.Log.WithName("controller").WithValues("kind", "ConstraintTemplate")

type Adder struct {
	Opa          opa.Client
	WatchManager *watch.WatchManager
}

// Add creates a new ConstraintTemplate Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func (a *Adder) Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr, a.Opa, a.WatchManager)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, opa opa.Client, wm *watch.WatchManager) (reconcile.Reconciler, error) {
	constraintAdder := constraint.Adder{Opa: opa}
	w, err := wm.NewRegistrar(
		ctrlName,
		[]func(manager.Manager, string) error{constraintAdder.Add})
	if err != nil {
		return nil, err
	}
	return &ReconcileConstraintTemplate{
		Client:  mgr.GetClient(),
		scheme:  mgr.GetScheme(),
		opa:     opa,
		watcher: w,
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(ctrlName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to ConstraintTemplate
	err = c.Watch(&source.Kind{Type: &v1alpha1.ConstraintTemplate{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create
	// Uncomment watch a Deployment created by ConstraintTemplate - change this for objects you create
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &v1alpha1.ConstraintTemplate{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileConstraintTemplate{}

// ReconcileConstraintTemplate reconciles a ConstraintTemplate object
type ReconcileConstraintTemplate struct {
	client.Client
	scheme  *runtime.Scheme
	watcher *watch.Registrar
	opa     opa.Client
}

// Reconcile reads that state of the cluster for a ConstraintTemplate object and makes changes based on the state read
// and what is in the ConstraintTemplate.Spec
// Automatically generate RBAC rules to allow the Controller to read and write CRDs
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=templates.gatekeeper.sh,resources=constrainttemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=templates.gatekeeper.sh,resources=constrainttemplates/status,verbs=get;update;patch
func (r *ReconcileConstraintTemplate) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the ConstraintTemplate instance
	instance := &v1alpha1.ConstraintTemplate{}
	err := r.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	crd, err := r.opa.CreateCRD(context.Background(), instance)
	if err != nil {
		return reconcile.Result{}, err
	}
	name := crd.GetName()
	namespace := crd.GetNamespace()
	if instance.GetDeletionTimestamp().IsZero() {
		// Check if the constraint already exists
		found := &apiextensionsv1beta1.CustomResourceDefinition{}
		err = r.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, found)
		if err != nil && errors.IsNotFound(err) {
			return r.handleCreate(instance, crd)

		} else if err != nil {
			return reconcile.Result{}, err

		} else {
			return r.handleUpdate(instance, crd, found)
		}

	}
	return r.handleDelete(instance, crd)
}

func (r *ReconcileConstraintTemplate) handleCreate(
	instance *v1alpha1.ConstraintTemplate,
	crd *apiextensionsv1beta1.CustomResourceDefinition) (reconcile.Result, error) {
	name := crd.GetName()
	log := log.WithValues("name", name)
	log.Info("creating constraint")
	if !containsString(finalizerName, instance.GetFinalizers()) {
		instance.SetFinalizers(append(instance.GetFinalizers(), finalizerName))
		if err := r.Update(context.Background(), instance); err != nil {
			log.Error(err, "update error", err)
			return reconcile.Result{Requeue: true}, nil
		}
	}
	log.Info("loading code into OPA")
	if _, err := r.opa.AddTemplate(context.Background(), instance); err != nil {
		return reconcile.Result{}, err
	}
	log.Info("adding to watcher registry")
	if err := r.watcher.AddWatch(instance.Spec.CRD.Spec.Names.Kind, name); err != nil {
		return reconcile.Result{}, err
	}
	log.Info("creating constraint CRD")
	if err := r.Create(context.TODO(), crd); err != nil {
		instance.Status.Error = fmt.Sprintf("Could not create CRD: %s", err)
		if err2 := r.Update(context.Background(), instance); err2 != nil {
			err = errorpkg.Wrap(err, fmt.Sprintf("Could not update status: %s", err2))
		}
		return reconcile.Result{}, err
	}
	instance.Status.Created = true
	if err := r.Update(context.Background(), instance); err != nil {
		return reconcile.Result{Requeue: true}, nil
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileConstraintTemplate) handleUpdate(
	instance *v1alpha1.ConstraintTemplate,
	crd, found *apiextensionsv1beta1.CustomResourceDefinition) (reconcile.Result, error) {
	// TODO: We may want to only check in code if it has changed. This is harder to do than it sounds
	// because even if the hash hasn't changed, OPA may have been restarted and needs code re-loaded
	// anyway. We should see if the OPA server is smart enough to look for changes on its own, otherwise
	// this may be too expensive to do in large clusters
	name := crd.GetName()
	log := log.WithValues("name", instance.GetName(), "crdName", name)
	log.Info("loading constraint code into OPA")
	if _, err := r.opa.AddTemplate(context.Background(), instance); err != nil {
		return reconcile.Result{}, err
	}
	log.Info("making sure constraint is in watcher registry")
	if err := r.watcher.AddWatch(instance.Spec.CRD.Spec.Names.Kind, name); err != nil {
		log.Error(err, "error adding template to watch registry")
		return reconcile.Result{}, err
	}
	if !reflect.DeepEqual(crd.Spec, found.Spec) {
		log.Info("difference in spec found, updating")
		found.Spec = crd.Spec
		if err := r.Update(context.Background(), found); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileConstraintTemplate) handleDelete(
	instance *v1alpha1.ConstraintTemplate,
	crd *apiextensionsv1beta1.CustomResourceDefinition) (reconcile.Result, error) {
	name := crd.GetName()
	namespace := crd.GetNamespace()
	log := log.WithValues("name", instance.GetName(), "crdName", name)
	if containsString(finalizerName, instance.GetFinalizers()) {
		if err := r.Delete(context.Background(), crd); err != nil && !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		found := &apiextensionsv1beta1.CustomResourceDefinition{}
		if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, found); err == nil {
			log.Info("child constraint CRD has not yet been deleted, waiting")
			return reconcile.Result{Requeue: true}, nil
		} else if err != nil && !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		log.Info("removing from watcher registry")
		if err := r.watcher.RemoveWatch(instance.Spec.CRD.Spec.Names.Kind); err != nil {
			return reconcile.Result{}, err
		}
		if _, err := r.opa.RemoveTemplate(context.Background(), instance); err != nil {
			return reconcile.Result{}, err
		}
		instance.SetFinalizers(removeString(finalizerName, instance.GetFinalizers()))
		if err := r.Update(context.Background(), instance); err != nil {
			return reconcile.Result{Requeue: true}, nil
		}
	}
	return reconcile.Result{}, nil
}

func containsString(s string, items []string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(s string, items []string) []string {
	var rval []string
	for _, item := range items {
		if item != s {
			rval = append(rval, item)
		}
	}
	return rval
}
